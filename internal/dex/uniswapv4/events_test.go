package uniswapv4

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func modifyLiquidityData(lower, upper int64, delta *big.Int) []byte {
	data := append(signedWordData(lower), signedWordData(upper)...)
	data = append(data, signedWordDataBig(delta)...)
	return append(data, make([]byte, abiWordSize)...)
}

func modifyLiquidityTopics(poolID domain.Hash) []domain.Hash {
	return []domain.Hash{modifyLiquidityTopic0, poolID, domain.Hash{0x0d}}
}

func TestDecodeModifyLiquidity(t *testing.T) {
	tests := []struct {
		name    string
		topics  []domain.Hash
		data    []byte
		want    LiquidityEvent
		ok      bool
		wantErr bool
	}{
		{
			name:   "mint",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, big.NewInt(1_000)),
			want: LiquidityEvent{
				PoolID:         testPoolID,
				TickLower:      -120,
				TickUpper:      120,
				LiquidityDelta: big.NewInt(1_000),
			},
			ok: true,
		},
		{
			name:   "burn",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, -60, big.NewInt(-500)),
			want: LiquidityEvent{
				PoolID:         testPoolID,
				TickLower:      -120,
				TickUpper:      -60,
				LiquidityDelta: big.NewInt(-500),
			},
			ok: true,
		},
		{
			name:   "zero delta",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, new(big.Int)),
			want: LiquidityEvent{
				PoolID:         testPoolID,
				TickLower:      -120,
				TickUpper:      120,
				LiquidityDelta: new(big.Int),
			},
			ok: true,
		},
		{
			name:   "minimum int128 delta",
			topics: modifyLiquidityTopics(testPoolID),
			data:   modifyLiquidityData(-120, 120, new(big.Int).Neg(pow2(127))),
			want: LiquidityEvent{
				PoolID:         testPoolID,
				TickLower:      -120,
				TickUpper:      120,
				LiquidityDelta: new(big.Int).Neg(pow2(127)),
			},
			ok: true,
		},
		{
			name:   "unknown topic",
			topics: []domain.Hash{swapTopic0, testPoolID},
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
			name:    "short data",
			topics:  modifyLiquidityTopics(testPoolID),
			data:    modifyLiquidityData(-120, 120, big.NewInt(1))[:abiWordSize*3],
			wantErr: true,
		},
		{
			name:    "long data",
			topics:  modifyLiquidityTopics(testPoolID),
			data:    append(modifyLiquidityData(-120, 120, big.NewInt(1)), make([]byte, abiWordSize)...),
			wantErr: true,
		},
		{
			name:    "lower outside int24",
			topics:  modifyLiquidityTopics(testPoolID),
			data:    modifyLiquidityData(1<<23, 120, big.NewInt(1)),
			wantErr: true,
		},
		{
			name:    "upper below int24",
			topics:  modifyLiquidityTopics(testPoolID),
			data:    modifyLiquidityData(-120, -1<<23-1, big.NewInt(1)),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := DecodeModifyLiquidity(test.topics, test.data)
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
				t.Fatalf("DecodeModifyLiquidity: %v", err)
			}
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if !test.ok {
				return
			}
			if got.PoolID != test.want.PoolID || got.TickLower != test.want.TickLower || got.TickUpper != test.want.TickUpper {
				t.Fatalf("event = %+v, want %+v", got, test.want)
			}
			if got.LiquidityDelta.Cmp(test.want.LiquidityDelta) != 0 {
				t.Fatalf("delta = %s, want %s", got.LiquidityDelta, test.want.LiquidityDelta)
			}
		})
	}
}

func TestApplyModifyLiquidityMintAndBurn(t *testing.T) {
	state := applyTestState()
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}}

	applied, err := Apply(&state, &deep, modifyLiquidityTopics(testPoolID), modifyLiquidityData(-120, 120, big.NewInt(1_000)))
	if err != nil {
		t.Fatalf("Apply mint: %v", err)
	}
	if !applied {
		t.Fatal("mint was not applied")
	}
	if got, want := state.Liquidity.Int64(), int64(1_001_000); got != want {
		t.Fatalf("active liquidity = %d, want %d", got, want)
	}
	assertDeepTick(t, deep, -120, 1_000, 1_000)
	assertDeepTick(t, deep, 120, 1_000, -1_000)

	applied, err = Apply(&state, &deep, modifyLiquidityTopics(testPoolID), modifyLiquidityData(-120, 120, big.NewInt(-400)))
	if err != nil {
		t.Fatalf("Apply burn: %v", err)
	}
	if !applied {
		t.Fatal("burn was not applied")
	}
	if got, want := state.Liquidity.Int64(), int64(1_000_600); got != want {
		t.Fatalf("active liquidity = %d, want %d", got, want)
	}
	assertDeepTick(t, deep, -120, 600, 600)
	assertDeepTick(t, deep, 120, 600, -600)
}

func TestApplyModifyLiquidityNegativeTicks(t *testing.T) {
	state := applyTestState()
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}}

	if _, err := Apply(&state, &deep, modifyLiquidityTopics(testPoolID), modifyLiquidityData(-120, -60, big.NewInt(500))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The active tick 0 is outside the range, so liquidity is unchanged.
	if got, want := state.Liquidity.Int64(), int64(1_000_000); got != want {
		t.Fatalf("active liquidity = %d, want %d", got, want)
	}
	assertDeepTick(t, deep, -120, 500, 500)
	assertDeepTick(t, deep, -60, 500, -500)
	// compressed tick -12 maps to word -1, bit 244.
	if word := deep.Bitmap[-1]; word == nil || word.Bit(244) != 1 {
		t.Fatalf("bitmap[-1] = %v, want bit 244", word)
	}
}

func TestApplyModifyLiquidityZeroDeltaIsNoOp(t *testing.T) {
	state := applyTestState()
	deep := pool.DeepState{
		Bitmap: map[int16]*big.Int{},
		Ticks:  map[int32]pool.TickInfo{},
	}
	beforeState := state.Copy()
	beforeDeep := deep.Copy()

	applied, err := Apply(&state, &deep, modifyLiquidityTopics(testPoolID), modifyLiquidityData(-120, 120, new(big.Int)))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("zero-delta log was not applied")
	}
	if !reflect.DeepEqual(state, beforeState) || !reflect.DeepEqual(deep, beforeDeep) {
		t.Fatal("zero-delta log changed state")
	}
}

func TestApplySwap(t *testing.T) {
	state := applyTestState()
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}}
	sqrtPrice := new(big.Int).Add(q96(), big.NewInt(1))

	applied, err := Apply(&state, &deep, []domain.Hash{swapTopic0, testPoolID}, swapData(sqrtPrice, big.NewInt(900), 120))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("swap was not applied")
	}
	if state.SqrtPriceX96.Cmp(sqrtPrice) != 0 || state.Liquidity.Int64() != 900 || state.Tick != 120 {
		t.Fatalf("swap state = (%s, %d, %d), want (%s, 900, 120)",
			state.SqrtPriceX96, state.Liquidity, state.Tick, sqrtPrice)
	}
	// The quote's fee/protocol-fee fields are independent of the swap log.
	if state.Fee != 500 || state.ProtocolFee != 0x07d07d {
		t.Fatalf("fees = %d/%#x, want 500/0x7d07d", state.Fee, state.ProtocolFee)
	}
}

func TestApplyUnknownTopic(t *testing.T) {
	state := applyTestState()
	deep := pool.DeepState{}
	applied, err := Apply(&state, &deep, []domain.Hash{unrelatedTopic}, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied {
		t.Fatal("unknown topic was applied")
	}
}

func TestApplyMalformedData(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
		data   []byte
	}{
		{name: "short swap", topics: []domain.Hash{swapTopic0, testPoolID}, data: make([]byte, abiWordSize)},
		{name: "short modify liquidity", topics: modifyLiquidityTopics(testPoolID), data: make([]byte, abiWordSize*3)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := applyTestState()
			deep := pool.DeepState{}
			applied, err := Apply(&state, &deep, test.topics, test.data)
			if !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("error = %v, want ErrInvalidLog", err)
			}
			if applied {
				t.Fatal("malformed log was applied")
			}
		})
	}
}

func TestApplyRejectsNilState(t *testing.T) {
	topics := modifyLiquidityTopics(testPoolID)
	data := modifyLiquidityData(-120, 120, big.NewInt(1))
	if _, err := Apply(nil, nil, topics, data); err == nil {
		t.Fatal("Apply with nil state returned no error")
	}
}

func applyTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: poolManagerAddress,
			Kind:    domain.PoolKindV4,
			PoolID:  testPoolID,
		},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          500,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: q96(),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000),
	}
}
