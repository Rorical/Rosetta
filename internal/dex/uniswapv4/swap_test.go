package uniswapv4

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

var (
	testPoolID            = domain.Hash{0xab, 0xcd}
	swapTopic0            = domain.Hash{0x40, 0xe9, 0xce, 0xcb}
	modifyLiquidityTopic0 = domain.Hash{0xf2, 0x08, 0xf4, 0x91}
	unrelatedTopic        = domain.Hash{0x00, 0x01, 0x02, 0x03}
)

func q96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

func pow2(bits uint) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), bits)
}

func word(value *big.Int) []byte {
	out := make([]byte, abiWordSize)
	value.FillBytes(out)
	return out
}

func signedWordData(value int64) []byte {
	encoded := big.NewInt(value)
	if value < 0 {
		encoded.Add(encoded, twoTo256)
	}
	out := make([]byte, abiWordSize)
	encoded.FillBytes(out)
	return out
}

func swapData(sqrtPriceX96, liquidity *big.Int, tick int64) []byte {
	data := append(word(big.NewInt(0)), word(big.NewInt(0))...)
	data = append(data, word(sqrtPriceX96)...)
	data = append(data, word(liquidity)...)
	data = append(data, signedWordData(tick)...)
	data = append(data, word(big.NewInt(500))...)
	return data
}

func TestDecodeSwap(t *testing.T) {
	tests := []struct {
		name    string
		topics  []domain.Hash
		data    []byte
		want    SwapEvent
		ok      bool
		wantErr bool
	}{
		{
			name:   "positive tick",
			topics: []domain.Hash{swapTopic0, testPoolID},
			data:   swapData(q96(), big.NewInt(1_000_000), 120),
			want: SwapEvent{
				PoolID:       testPoolID,
				SqrtPriceX96: q96(),
				Liquidity:    big.NewInt(1_000_000),
				Tick:         120,
			},
			ok: true,
		},
		{
			name:   "negative tick",
			topics: []domain.Hash{swapTopic0, testPoolID},
			data:   swapData(q96(), big.NewInt(7), -887272),
			want: SwapEvent{
				PoolID:       testPoolID,
				SqrtPriceX96: q96(),
				Liquidity:    big.NewInt(7),
				Tick:         -887272,
			},
			ok: true,
		},
		{
			name:   "minimum int24 tick",
			topics: []domain.Hash{swapTopic0, testPoolID},
			data:   swapData(q96(), big.NewInt(1), -1<<23),
			want: SwapEvent{
				PoolID:       testPoolID,
				SqrtPriceX96: q96(),
				Liquidity:    big.NewInt(1),
				Tick:         -1 << 23,
			},
			ok: true,
		},
		{
			name:   "trailing topic is ignored",
			topics: []domain.Hash{swapTopic0, testPoolID, unrelatedTopic},
			data:   swapData(q96(), big.NewInt(3), 5),
			want: SwapEvent{
				PoolID:       testPoolID,
				SqrtPriceX96: q96(),
				Liquidity:    big.NewInt(3),
				Tick:         5,
			},
			ok: true,
		},
		{
			name:   "unknown topic0",
			topics: []domain.Hash{modifyLiquidityTopic0, testPoolID},
			data:   swapData(q96(), big.NewInt(1), 0),
			ok:     false,
		},
		{
			name:    "missing pool id",
			topics:  []domain.Hash{swapTopic0},
			data:    swapData(q96(), big.NewInt(1), 0),
			wantErr: true,
		},
		{
			name:    "short data",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    swapData(q96(), big.NewInt(1), 0)[:abiWordSize*5],
			wantErr: true,
		},
		{
			name:    "long data",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    append(swapData(q96(), big.NewInt(1), 0), make([]byte, abiWordSize)...),
			wantErr: true,
		},
		{
			name:    "sqrt price wider than uint160",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    swapData(pow2(160), big.NewInt(1), 0),
			wantErr: true,
		},
		{
			name:    "liquidity wider than uint128",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    swapData(q96(), pow2(128), 0),
			wantErr: true,
		},
		{
			name:    "tick above int24",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    swapData(q96(), big.NewInt(1), 1<<23),
			wantErr: true,
		},
		{
			name:    "tick below int24",
			topics:  []domain.Hash{swapTopic0, testPoolID},
			data:    swapData(q96(), big.NewInt(1), -1<<23-1),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := DecodeSwap(test.topics, test.data)
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
				t.Fatalf("DecodeSwap: %v", err)
			}
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if !test.ok {
				if got != (SwapEvent{}) {
					t.Fatalf("event = %+v, want zero", got)
				}
				return
			}
			if got.PoolID != test.want.PoolID {
				t.Fatalf("pool id = %s, want %s", got.PoolID.Hex(), test.want.PoolID.Hex())
			}
			if got.SqrtPriceX96.Cmp(test.want.SqrtPriceX96) != 0 {
				t.Fatalf("sqrt price = %s, want %s", got.SqrtPriceX96, test.want.SqrtPriceX96)
			}
			if got.Liquidity.Cmp(test.want.Liquidity) != 0 {
				t.Fatalf("liquidity = %s, want %s", got.Liquidity, test.want.Liquidity)
			}
			if got.Tick != test.want.Tick {
				t.Fatalf("tick = %d, want %d", got.Tick, test.want.Tick)
			}
		})
	}
}

func TestTopicConstants(t *testing.T) {
	topics := map[string]string{
		"swap":             TopicSwap,
		"modify liquidity": TopicModifyLiquidity,
	}
	for name, topic := range topics {
		raw := strings.TrimPrefix(topic, "0x")
		if len(raw) != 2*topicSelectorLen {
			t.Fatalf("%s topic = %q, want %d hex characters", name, topic, 2*topicSelectorLen)
		}
		if _, err := hex.DecodeString(raw); err != nil {
			t.Fatalf("%s topic = %q: %v", name, topic, err)
		}
	}
	if len(swapTopic) != topicSelectorLen {
		t.Fatalf("decoded swap selector length = %d, want %d", len(swapTopic), topicSelectorLen)
	}
}
