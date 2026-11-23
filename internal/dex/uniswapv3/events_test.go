package uniswapv3

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestApplyInitialize(t *testing.T) {
	state := &pool.State{
		Key:          domain.PoolKey{Kind: domain.PoolKindV3},
		Tick:         30,
		Liquidity:    big.NewInt(77),
		SqrtPriceX96: big.NewInt(99),
	}
	deep := &pool.DeepState{}
	price := new(big.Int).Set(math.Q96)

	if err := Apply(state, deep, Event{Kind: EventInitialize, SqrtPriceX96: price, Tick: -12}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if state.Tick != -12 {
		t.Fatalf("tick = %d, want -12", state.Tick)
	}
	if state.SqrtPriceX96.Cmp(math.Q96) != 0 {
		t.Fatalf("sqrt price = %s, want %s", state.SqrtPriceX96, math.Q96)
	}
	if state.Liquidity == nil || state.Liquidity.Sign() != 0 {
		t.Fatalf("liquidity = %v, want zero", state.Liquidity)
	}
	if state.SqrtPriceX96 == price {
		t.Fatalf("initialize did not clone sqrt price")
	}
	price.SetInt64(1)
	if state.SqrtPriceX96.Cmp(math.Q96) != 0 {
		t.Fatalf("state shares the event sqrt price")
	}
}

func TestApplyMint(t *testing.T) {
	tests := []struct {
		name           string
		tick           int32
		lower          int32
		upper          int32
		amount         int64
		startLiquidity int64
		nilLiquidity   bool
		wantLiquidity  int64
	}{
		{"in range", 0, -60, 60, 1000, 500, false, 1500},
		{"lower boundary included", 0, 0, 60, 1000, 500, false, 1500},
		{"upper boundary excluded", 0, -60, 0, 1000, 500, false, 500},
		{"above range", 0, 60, 120, 1000, 500, false, 500},
		{"below range", 0, -120, -60, 1000, 500, false, 500},
		{"nil active liquidity", 0, -60, 60, 1000, 0, true, 1000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &pool.State{Tick: test.tick, TickSpacing: 60}
			if !test.nilLiquidity {
				state.Liquidity = big.NewInt(test.startLiquidity)
			}
			deep := &pool.DeepState{}

			err := Apply(state, deep, Event{
				Kind:      EventMint,
				TickLower: test.lower,
				TickUpper: test.upper,
				Amount:    big.NewInt(test.amount),
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			assertLiquidity(t, state.Liquidity, test.wantLiquidity)
			if deep.Bitmap == nil || deep.Ticks == nil {
				t.Fatalf("mint left a nil map")
			}
			assertTick(t, deep, test.lower, test.amount, test.amount)
			assertTick(t, deep, test.upper, test.amount, -test.amount)
		})
	}
}

func TestApplyMintAccumulates(t *testing.T) {
	state := &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}
	deep := &pool.DeepState{}
	for i := 0; i < 2; i++ {
		if err := Apply(state, deep, Event{
			Kind:      EventMint,
			TickLower: -60,
			TickUpper: 60,
			Amount:    big.NewInt(300),
		}); err != nil {
			t.Fatalf("Apply mint %d: %v", i, err)
		}
	}
	assertLiquidity(t, state.Liquidity, 600)
	assertTick(t, deep, -60, 600, 600)
	assertTick(t, deep, 60, 600, -600)
}

func TestApplyMintErrors(t *testing.T) {
	tests := []struct {
		name   string
		lower  int32
		upper  int32
		amount *big.Int
	}{
		{"equal ticks", 0, 0, big.NewInt(1)},
		{"inverted ticks", 60, -60, big.NewInt(1)},
		{"nil amount", -60, 60, nil},
		{"negative amount", -60, 60, big.NewInt(-1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}
			err := Apply(state, &pool.DeepState{}, Event{
				Kind:      EventMint,
				TickLower: test.lower,
				TickUpper: test.upper,
				Amount:    test.amount,
			})
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

// TestApplyZeroAmountIsNoOp checks that zero-amount mints and burns, which the
// chain emits when a position update changes no liquidity, succeed without
// touching shallow or deep state.
func TestApplyZeroAmountIsNoOp(t *testing.T) {
	tests := []struct {
		name  string
		kind  EventKind
		setup func(t *testing.T) (*pool.State, *pool.DeepState)
	}{
		{
			name:  "mint with loaded ticks",
			kind:  EventMint,
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) { return mintedPosition(t, 0, -60, 60, 1000) },
		},
		{
			name:  "burn with loaded ticks",
			kind:  EventBurn,
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) { return mintedPosition(t, 0, -60, 60, 1000) },
		},
		{"mint without deep state", EventMint, emptyPosition},
		{"burn without deep state", EventBurn, emptyPosition},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, deep := test.setup(t)
			beforeState := state.Copy()
			beforeDeep := deep.Copy()

			err := Apply(state, deep, Event{
				Kind:      test.kind,
				TickLower: -60,
				TickUpper: 60,
				Amount:    new(big.Int),
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !reflect.DeepEqual(*state, beforeState) {
				t.Fatalf("zero amount mutated state: %+v, want %+v", *state, beforeState)
			}
			if !reflect.DeepEqual(*deep, beforeDeep) {
				t.Fatalf("zero amount mutated deep: %+v, want %+v", *deep, beforeDeep)
			}
		})
	}
}

func mintedPosition(t *testing.T, tick, lower, upper int32, amount int64) (*pool.State, *pool.DeepState) {
	t.Helper()
	state := &pool.State{Tick: tick, TickSpacing: 60, Liquidity: new(big.Int)}
	deep := &pool.DeepState{}
	err := Apply(state, deep, Event{
		Kind:      EventMint,
		TickLower: lower,
		TickUpper: upper,
		Amount:    big.NewInt(amount),
	})
	if err != nil {
		t.Fatalf("mint setup: %v", err)
	}
	return state, deep
}

func emptyPosition(t *testing.T) (*pool.State, *pool.DeepState) {
	t.Helper()
	return &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}, &pool.DeepState{}
}

func assertLiquidity(t *testing.T, got *big.Int, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("liquidity is nil, want %d", want)
	}
	if got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("liquidity = %s, want %d", got, want)
	}
}

func assertTick(t *testing.T, deep *pool.DeepState, tick int32, gross, net int64) {
	t.Helper()
	info, ok := deep.Ticks[tick]
	if !ok {
		t.Fatalf("tick %d missing", tick)
	}
	if info.LiquidityGross == nil || info.LiquidityNet == nil {
		t.Fatalf("tick %d has nil liquidity", tick)
	}
	if info.LiquidityGross.Cmp(big.NewInt(gross)) != 0 {
		t.Fatalf("tick %d gross = %s, want %d", tick, info.LiquidityGross, gross)
	}
	if info.LiquidityNet.Cmp(big.NewInt(net)) != 0 {
		t.Fatalf("tick %d net = %s, want %d", tick, info.LiquidityNet, net)
	}
}

func bitmapBitSet(bitmap map[int16]*big.Int, tick, tickSpacing int32) bool {
	wordPos, bitPos := tickBitmapPosition(compressTick(tick, tickSpacing))
	word := bitmap[wordPos]
	return word != nil && word.Bit(bitPos) == 1
}

func TestApplyInitializeResetsDeepState(t *testing.T) {
	state := &pool.State{
		Key:          domain.PoolKey{Kind: domain.PoolKindV3},
		Tick:         30,
		Liquidity:    big.NewInt(77),
		SqrtPriceX96: big.NewInt(99),
	}
	oldBitmap := map[int16]*big.Int{0: big.NewInt(1)}
	oldTicks := map[int32]pool.TickInfo{-60: {LiquidityGross: big.NewInt(1), LiquidityNet: big.NewInt(1)}}
	deep := &pool.DeepState{
		Bitmap:    oldBitmap,
		Ticks:     oldTicks,
		WindowMin: -2560,
		WindowMax: 2559,
	}

	if err := Apply(state, deep, Event{Kind: EventInitialize, SqrtPriceX96: new(big.Int).Set(math.Q96), Tick: 0}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if deep.Bitmap == nil || len(deep.Bitmap) != 0 {
		t.Fatalf("bitmap = %v, want empty non-nil map", deep.Bitmap)
	}
	if deep.Ticks == nil || len(deep.Ticks) != 0 {
		t.Fatalf("ticks = %v, want empty non-nil map", deep.Ticks)
	}
	if deep.WindowMin != 0 || deep.WindowMax != 0 {
		t.Fatalf("window = [%d, %d], want cleared", deep.WindowMin, deep.WindowMax)
	}
	if len(oldBitmap) != 1 || len(oldTicks) != 1 {
		t.Fatalf("initialize reused the previous maps")
	}
}

func TestApplyMintSetsBitmapAndQuoteCrossesTick(t *testing.T) {
	const tickSpacing int32 = 10
	state := quoteTestState(domain.PoolKindV3)
	state.TickSpacing = tickSpacing
	state.Liquidity = new(big.Int)
	deep := &pool.DeepState{}

	mint := func(lower, upper int32, amount int64) {
		t.Helper()
		if err := Apply(&state, deep, Event{
			Kind:      EventMint,
			TickLower: lower,
			TickUpper: upper,
			Amount:    big.NewInt(amount),
		}); err != nil {
			t.Fatalf("mint %d..%d: %v", lower, upper, err)
		}
	}
	mint(-20, 20, 2_000_000_000_000_000_000)
	mint(-40, -20, 1_000_000_000_000_000_000)

	for _, tick := range []int32{-40, -20, 20} {
		if !bitmapBitSet(deep.Bitmap, tick, tickSpacing) {
			t.Fatalf("tick %d bitmap bit not set", tick)
		}
	}

	adapter, err := New(state.Fee, tickSpacing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	amountIn := big.NewInt(2_500_000_000_000_000)
	got, err := adapter.QuoteExactIn(context.Background(), state, *deep, testToken0, amountIn)
	if err != nil {
		t.Fatalf("QuoteExactIn: %v", err)
	}

	v3 := math.V3State{
		SqrtPriceX96: state.SqrtPriceX96,
		Tick:         state.Tick,
		Liquidity:    state.Liquidity,
		FeePips:      state.Fee,
		TickSpacing:  state.TickSpacing,
		Bitmap:       deep.Bitmap,
		Ticks:        deep.Ticks,
	}
	want, err := math.SwapV3ExactIn(v3, true, amountIn)
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	if len(want.Crossed) == 0 || want.Crossed[0] != -20 {
		t.Fatalf("crossed = %v, want -20 first", want.Crossed)
	}
	if got.Cmp(want.AmountOut) != 0 {
		t.Fatalf("quote = %s, want %s", got, want.AmountOut)
	}

	withoutBitmap := v3
	withoutBitmap.Bitmap = nil
	uncrossed, err := math.SwapV3ExactIn(withoutBitmap, true, amountIn)
	if err != nil {
		t.Fatalf("SwapV3ExactIn without bitmap: %v", err)
	}
	if uncrossed.AmountOut.Cmp(want.AmountOut) == 0 {
		t.Fatalf("quote is unchanged without the minted bitmap")
	}
}

func TestApplyMintBitmapCompressesNegativeTick(t *testing.T) {
	state := &pool.State{Tick: 0, TickSpacing: 1, Liquidity: new(big.Int)}
	deep := &pool.DeepState{}

	if err := Apply(state, deep, Event{
		Kind:      EventMint,
		TickLower: -1,
		TickUpper: 0,
		Amount:    big.NewInt(100),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	lowerWord := deep.Bitmap[-1]
	if lowerWord == nil || lowerWord.Bit(255) != 1 {
		t.Fatalf("tick -1 word = %v, want bit 255 in word -1", lowerWord)
	}
	upperWord := deep.Bitmap[0]
	if upperWord == nil || upperWord.Bit(0) != 1 {
		t.Fatalf("tick 0 word = %v, want bit 0 in word 0", upperWord)
	}
}

func TestCompressTickFloorDivision(t *testing.T) {
	tests := []struct {
		tick        int32
		tickSpacing int32
		wantWord    int16
		wantBit     int
	}{
		{-1, 1, -1, 255},
		{-15, 10, -1, 254},
		{-10, 10, -1, 255},
		{0, 10, 0, 0},
		{255, 1, 0, 255},
		{256, 1, 1, 0},
	}
	for _, test := range tests {
		wordPos, bitPos := tickBitmapPosition(compressTick(test.tick, test.tickSpacing))
		if wordPos != test.wantWord || bitPos != test.wantBit {
			t.Fatalf("tick %d spacing %d -> word %d bit %d, want word %d bit %d",
				test.tick, test.tickSpacing, wordPos, bitPos, test.wantWord, test.wantBit)
		}
	}
}

func TestApplyRejectsInvalidTicks(t *testing.T) {
	tests := []struct {
		name    string
		lower   int32
		upper   int32
		spacing int32
	}{
		{"below min tick", math.MinTick - 1, 0, 1},
		{"above max tick", 0, math.MaxTick + 1, 1},
		{"lower not aligned", -15, 10, 10},
		{"upper not aligned", -20, 15, 10},
		{"non-positive spacing", -20, 20, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &pool.State{Tick: 0, TickSpacing: test.spacing, Liquidity: new(big.Int)}
			err := Apply(state, &pool.DeepState{}, Event{
				Kind:      EventMint,
				TickLower: test.lower,
				TickUpper: test.upper,
				Amount:    big.NewInt(1),
			})
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("error = %v, want ErrInvalidEvent", err)
			}
			if test.spacing > 0 && !strings.Contains(err.Error(), "outside") && !strings.Contains(err.Error(), "not aligned") {
				t.Fatalf("error = %v, want a range or alignment failure", err)
			}
		})
	}
}
