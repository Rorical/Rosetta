package uniswapv4

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestDecodeEvent(t *testing.T) {
	sqrtPrice := new(big.Int).Add(q96(), big.NewInt(1))
	tests := []struct {
		name    string
		topics  []domain.Hash
		data    []byte
		want    uniswapv3.Event
		ok      bool
		wantErr bool
	}{
		{
			name:   "swap",
			topics: []domain.Hash{swapTopic0, testPoolID},
			data:   swapData(sqrtPrice, big.NewInt(900), 120),
			want: uniswapv3.Event{
				Kind:         uniswapv3.EventSwap,
				SqrtPriceX96: sqrtPrice,
				Liquidity:    big.NewInt(900),
				Tick:         120,
			},
			ok: true,
		},
		{
			name:   "modify liquidity mint",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, big.NewInt(1_000)),
			want: uniswapv3.Event{
				Kind:      uniswapv3.EventMint,
				TickLower: -120,
				TickUpper: 120,
				Amount:    big.NewInt(1_000),
			},
			ok: true,
		},
		{
			name:   "modify liquidity burn",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, -60, big.NewInt(-500)),
			want: uniswapv3.Event{
				Kind:      uniswapv3.EventBurn,
				TickLower: -120,
				TickUpper: -60,
				Amount:    big.NewInt(500),
			},
			ok: true,
		},
		{
			name:   "modify liquidity zero delta",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, new(big.Int)),
			want:   uniswapv3.Event{Kind: uniswapv3.EventCollect},
			ok:     true,
		},
		{
			name:   "unknown topic",
			topics: []domain.Hash{unrelatedTopic, testPoolID},
			data:   modifyLiquidityData(-120, 120, big.NewInt(1)),
			ok:     false,
		},
		{
			name:    "missing pool id",
			topics:  []domain.Hash{modifyLiquidityTopic0},
			data:    modifyLiquidityData(-120, 120, big.NewInt(1)),
			wantErr: true,
		},
		{
			name:    "short swap",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    make([]byte, abiWordSize),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := DecodeEvent(test.topics, test.data)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidLog) {
					t.Fatalf("error = %v, want ErrInvalidLog", err)
				}
				if ok {
					t.Fatal("ok = true on malformed log")
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeEvent: %v", err)
			}
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if !test.ok {
				return
			}
			assertEventEqual(t, got, test.want)
		})
	}
}

// TestDecodeEventMatchesApply checks that decoding a log and reducing the event
// with the V3 reducer reaches the same state as Apply, which decodes and
// reduces in one step.
func TestDecodeEventMatchesApply(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
		data   []byte
	}{
		{
			name:   "swap",
			topics: []domain.Hash{swapTopic0, testPoolID},
			data:   swapData(new(big.Int).Add(q96(), big.NewInt(1)), big.NewInt(900), 120),
		},
		{
			name:   "mint",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, big.NewInt(1_000)),
		},
		{
			name:   "burn",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, big.NewInt(-400)),
		},
		{
			name:   "zero delta",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, new(big.Int)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ev, ok, err := DecodeEvent(test.topics, test.data)
			if err != nil || !ok {
				t.Fatalf("DecodeEvent = (%v, %t, %v), want an event", ev, ok, err)
			}

			appliedState := applyTestState()
			appliedDeep := pool.DeepState{}
			applied, err := Apply(&appliedState, &appliedDeep, test.topics, test.data)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}

			reducedState := applyTestState()
			reducedDeep := pool.DeepState{}
			if err := uniswapv3.Apply(&reducedState, &reducedDeep, ev); err != nil {
				t.Fatalf("uniswapv3.Apply: %v", err)
			}

			if !applied {
				t.Fatal("Apply returned false for a decoded event")
			}
			if !reflect.DeepEqual(appliedState, reducedState) {
				t.Fatalf("state = %+v, want %+v", reducedState, appliedState)
			}
			if !reflect.DeepEqual(appliedDeep, reducedDeep) {
				t.Fatalf("deep = %+v, want %+v", reducedDeep, appliedDeep)
			}
		})
	}
}

func assertEventEqual(t *testing.T, got, want uniswapv3.Event) {
	t.Helper()
	if got.Kind != want.Kind || got.TickLower != want.TickLower || got.TickUpper != want.TickUpper || got.Tick != want.Tick {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
	for _, field := range []struct {
		name string
		got  *big.Int
		want *big.Int
	}{
		{"amount", got.Amount, want.Amount},
		{"sqrt price", got.SqrtPriceX96, want.SqrtPriceX96},
		{"liquidity", got.Liquidity, want.Liquidity},
	} {
		if (field.got == nil) != (field.want == nil) ||
			(field.got != nil && field.got.Cmp(field.want) != 0) {
			t.Fatalf("%s = %v, want %v", field.name, field.got, field.want)
		}
	}
}
