package math

import (
	"fmt"
	"math/big"
	"math/rand"
	"slices"
	"testing"

	"github.com/holiman/uint256"
)

// swapDifferential counts how often the fixed-width path handled a case
// itself, so a generator drifting into fallback-only inputs fails loudly
// instead of comparing the arbitrary-precision path with itself.
type swapDifferential struct {
	handled, declined, errors int
	// declinedResults counts declined cases that arbitrary precision
	// completed: the inputs or intermediates wider than a deployed pool allows.
	declinedResults int
	// crossings totals the initialized ticks crossed by handled swaps.
	crossings int
}

func (d *swapDifferential) String() string {
	return fmt.Sprintf("handled %d (%d tick crossings), declined %d (%d completed in arbitrary precision), errors %d",
		d.handled, d.crossings, d.declined, d.declinedResults, d.errors)
}

// compare runs one swap through the fixed-width path and through
// swapV3ExactInBig and requires the same result or the same error text. A
// declined case must also come back identical through the public entry point.
// protocolWidth asserts that every value the pool can reach fits the
// protocol's widths, so the fixed-width path must not decline a swap that
// completes.
func (d *swapDifferential) compare(t *testing.T, label string, state V3State, zeroForOne bool, amountIn, limit *big.Int, protocolWidth bool) {
	t.Helper()
	want, wantErr := swapV3ExactInBig(state, zeroForOne, amountIn, limit)
	got, handled, err := swapV3ExactIn256(state, zeroForOne, amountIn, limit)
	if handled {
		d.handled++
	} else {
		d.declined++
		got, err = SwapV3ExactInWithLimit(state, zeroForOne, amountIn, limit)
	}
	if fmt.Sprint(err) != fmt.Sprint(wantErr) {
		t.Fatalf("%s: handled=%v error %v, want %v", label, handled, err, wantErr)
	}
	if err != nil {
		d.errors++
		return
	}
	if handled {
		d.crossings += len(got.Crossed)
	} else {
		d.declinedResults++
		if protocolWidth {
			t.Fatalf("%s: declined a protocol-width swap", label)
		}
	}
	if !sameSwapResult(got, want) {
		t.Fatalf("%s: handled=%v\n got %s\nwant %s", label, handled, describeSwap(got), describeSwap(want))
	}
}

func sameSwapResult(a, b SwapResult) bool {
	return a.AmountOut.Cmp(b.AmountOut) == 0 && a.AmountInUsed.Cmp(b.AmountInUsed) == 0 &&
		a.SqrtPriceX96.Cmp(b.SqrtPriceX96) == 0 && a.Liquidity.Cmp(b.Liquidity) == 0 &&
		a.Tick == b.Tick && a.HitLimit == b.HitLimit && slices.Equal(a.Crossed, b.Crossed) &&
		(a.Crossed == nil) == (b.Crossed == nil)
}

func describeSwap(r SwapResult) string {
	return fmt.Sprintf("out=%s in=%s price=%s liquidity=%s tick=%d crossed=%v hit=%v",
		r.AmountOut, r.AmountInUsed, r.SqrtPriceX96, r.Liquidity, r.Tick, r.Crossed, r.HitLimit)
}

func pow2(n uint) *big.Int { return new(big.Int).Lsh(one, n) }

func pow2Minus1(n uint) *big.Int { return new(big.Int).Sub(pow2(n), one) }

// randomBits returns a uniform value of exactly bits bits, or zero for 0.
func randomBits(random *rand.Rand, bits int) *big.Int {
	if bits <= 0 {
		return new(big.Int)
	}
	value := new(big.Int).Rand(random, pow2(uint(bits-1)))
	return value.SetBit(value, bits-1, 1)
}

type randomPoolSpec struct {
	spacing   int32
	positions int
	span      int32 // ticks either side of the current tick that positions use
}

// randomV3State builds a pool from random liquidity positions, so tick nets
// and the active liquidity are consistent unless corrupt is set, which then
// damages one tick to exercise the error paths. protocolWidth reports that no
// liquidity the pool can reach leaves uint128.
func randomV3State(random *rand.Rand, spec randomPoolSpec, corrupt bool) (state V3State, protocolWidth bool) {
	tick := randomTick(random)
	lower := mustTickPrice(tick)
	var price *big.Int
	switch random.Intn(4) {
	case 0:
		price = new(big.Int).Set(lower)
	case 1:
		if tick < MaxTick {
			price = new(big.Int).Sub(mustTickPrice(tick+1), one)
		} else {
			price = new(big.Int).Set(lower)
		}
	default:
		price = new(big.Int).Set(lower)
		if tick < MaxTick {
			width := new(big.Int).Sub(mustTickPrice(tick+1), lower)
			price.Add(price, new(big.Int).Rand(random, width))
		}
	}
	if price.Cmp(maxSqrtRatioX96) >= 0 {
		tick, price = MaxTick-1, mustTickPrice(MaxTick-1)
	}

	total := new(big.Int)
	state = V3State{
		SqrtPriceX96: price,
		Tick:         tick,
		Liquidity:    new(big.Int),
		FeePips:      randomFee(random),
		TickSpacing:  spec.spacing,
		Bitmap:       make(map[int16]*big.Int),
		Ticks:        make(map[int32]TickInfo),
	}
	minUsable := -(-MinTick / spec.spacing) * spec.spacing
	maxUsable := (MaxTick / spec.spacing) * spec.spacing
	addPosition := func(lowerTick, upperTick int32, liquidity *big.Int) {
		lowerTick = max(minUsable, min(maxUsable, lowerTick))
		upperTick = max(minUsable, min(maxUsable, upperTick))
		if lowerTick >= upperTick || liquidity.Sign() == 0 {
			return
		}
		total.Add(total, liquidity)
		addTickNet(&state, lowerTick, liquidity)
		addTickNet(&state, upperTick, new(big.Int).Neg(liquidity))
		if lowerTick <= tick && tick < upperTick {
			state.Liquidity.Add(state.Liquidity, liquidity)
		}
	}
	// One position always covers the current tick so liquidity is positive.
	base := floorDiv(tick, spec.spacing) * spec.spacing
	addPosition(base-spec.spacing*int32(random.Intn(50)), base+spec.spacing*int32(1+random.Intn(50)), randomLiquidity(random))
	for range spec.positions {
		a := base + spec.spacing*int32(random.Intn(int(2*spec.span/spec.spacing)+1)-int(spec.span/spec.spacing))
		b := base + spec.spacing*int32(random.Intn(int(2*spec.span/spec.spacing)+1)-int(spec.span/spec.spacing))
		addPosition(min(a, b), max(a, b), randomLiquidity(random))
	}
	if random.Intn(8) == 0 {
		addPosition(minUsable, maxUsable, randomLiquidity(random))
	}
	if state.Liquidity.Sign() == 0 {
		state.Liquidity.SetInt64(1)
	}

	if random.Intn(3) == 0 {
		state.WindowMin = tick - int32(random.Intn(int(spec.span)+1))
		state.WindowMax = tick + int32(random.Intn(int(spec.span)+1))
	}
	if corrupt && len(state.Ticks) > 0 {
		ticks := make([]int32, 0, len(state.Ticks))
		for t := range state.Ticks {
			ticks = append(ticks, t)
		}
		slices.Sort(ticks)
		victim := ticks[random.Intn(len(ticks))]
		switch random.Intn(4) {
		case 0:
			delete(state.Ticks, victim)
		case 1:
			state.Ticks[victim] = TickInfo{}
		case 2:
			state.Ticks[victim] = TickInfo{LiquidityNet: new(big.Int).Neg(new(big.Int).Lsh(state.Liquidity, 2))}
		default:
			state.Ticks[victim] = TickInfo{LiquidityNet: new(big.Int).Lsh(one, uint(120+random.Intn(20)))}
		}
	}
	return state, !corrupt && total.BitLen() < 128
}

func addTickNet(state *V3State, tick int32, net *big.Int) {
	info := state.Ticks[tick]
	if info.LiquidityNet == nil {
		info = TickInfo{LiquidityGross: new(big.Int), LiquidityNet: new(big.Int)}
	}
	info.LiquidityGross.Add(info.LiquidityGross, new(big.Int).Abs(net))
	info.LiquidityNet.Add(info.LiquidityNet, net)
	state.Ticks[tick] = info
	compressed := floorDiv(tick, state.TickSpacing)
	word := state.Bitmap[int16(compressed>>8)]
	if word == nil {
		word = new(big.Int)
		state.Bitmap[int16(compressed>>8)] = word
	}
	word.SetBit(word, int(compressed&255), 1)
}

func randomTick(random *rand.Rand) int32 {
	switch random.Intn(5) {
	case 0:
		return MinTick + int32(random.Intn(3000))
	case 1:
		return MaxTick - 1 - int32(random.Intn(3000))
	case 2:
		return int32(random.Intn(2001) - 1000)
	default:
		return MinTick + int32(random.Intn(int(MaxTick-MinTick)))
	}
}

func randomFee(random *rand.Rand) uint32 {
	fees := []uint32{0, 1, 100, 500, 3000, 10000, 100000, 999999}
	if random.Intn(4) == 0 {
		return uint32(random.Intn(feePipsDenominator))
	}
	return fees[random.Intn(len(fees))]
}

// randomLiquidity is mostly below 2^110, so sums of positions usually stay
// inside uint128, with occasional values at the uint128 limit whose sums
// leave it.
func randomLiquidity(random *rand.Rand) *big.Int {
	switch random.Intn(100) {
	case 0:
		return big.NewInt(1)
	case 1:
		return pow2Minus1(128 - uint(random.Intn(3)))
	default:
		return randomBits(random, 1+random.Intn(110))
	}
}

func randomAmount(random *rand.Rand) *big.Int {
	switch random.Intn(12) {
	case 0:
		return big.NewInt(1)
	case 1:
		return pow2Minus1(255)
	case 2:
		return pow2Minus1(256)
	case 3:
		return pow2(uint(240 + random.Intn(16)))
	default:
		return randomBits(random, 1+random.Intn(200))
	}
}

// randomLimit returns a nil, zero, valid or invalid price limit.
func randomLimit(random *rand.Rand, state V3State, zeroForOne bool) *big.Int {
	price := state.SqrtPriceX96
	switch random.Intn(8) {
	case 0:
		return new(big.Int)
	case 1: // Invalid: the current price or beyond the range.
		switch random.Intn(3) {
		case 0:
			return new(big.Int).Set(price)
		case 1:
			return new(big.Int).Set(minSqrtRatioX96)
		default:
			return new(big.Int).Set(maxSqrtRatioX96)
		}
	case 2, 3: // Valid, between the bound and the current price.
		if zeroForOne {
			gap := new(big.Int).Sub(price, minSqrtRatioX96)
			if gap.Cmp(big.NewInt(2)) < 0 {
				return nil
			}
			gap.Sub(gap, one)
			return gap.Add(new(big.Int).Rand(random, gap), new(big.Int).Add(minSqrtRatioX96, one))
		}
		gap := new(big.Int).Sub(maxSqrtRatioX96, price)
		if gap.Cmp(big.NewInt(2)) < 0 {
			return nil
		}
		gap.Sub(gap, one)
		return gap.Add(new(big.Int).Rand(random, gap), new(big.Int).Add(price, one))
	case 4: // Valid, a few ticks away.
		target := state.Tick - int32(random.Intn(200))
		if !zeroForOne {
			target = state.Tick + 1 + int32(random.Intn(200))
		}
		limit := mustTickPrice(max(MinTick+1, min(MaxTick-1, target)))
		if zeroForOne && limit.Cmp(price) >= 0 || !zeroForOne && limit.Cmp(price) <= 0 {
			return nil
		}
		return limit
	default:
		return nil
	}
}

func mustTickPrice(tick int32) *big.Int {
	price, err := referenceTickPrice(tick)
	if err != nil {
		panic(err)
	}
	return price
}

func TestSwapV3ExactIn256MatchesBigOnRandomPools(t *testing.T) {
	random := rand.New(rand.NewSource(42161))
	specs := []randomPoolSpec{
		{spacing: 1, positions: 30, span: 400},
		{spacing: 10, positions: 40, span: 3000},
		{spacing: 60, positions: 25, span: 20000},
		{spacing: 200, positions: 10, span: 100000},
		{spacing: 16384, positions: 6, span: 400000},
	}
	var diff swapDifferential
	for i := range 1500 {
		spec := specs[i%len(specs)]
		state, protocolWidth := randomV3State(random, spec, i%11 == 0)
		for range 4 {
			zeroForOne := random.Intn(2) == 0
			amount := randomAmount(random)
			limit := randomLimit(random, state, zeroForOne)
			label := fmt.Sprintf("pool %d spacing %d tick %d price %s liquidity %s fee %d zeroForOne=%v amount %s limit %v window [%d, %d]",
				i, spec.spacing, state.Tick, state.SqrtPriceX96, state.Liquidity, state.FeePips, zeroForOne, amount, limit, state.WindowMin, state.WindowMax)
			diff.compare(t, label, state, zeroForOne, amount, limit, protocolWidth && amount.BitLen() <= 256)
		}
	}
	t.Log(&diff)
	if diff.handled < 2*diff.declined || diff.declinedResults == 0 {
		t.Fatalf("fixed-width path handled %d and declined %d cases; the generator no longer covers both", diff.handled, diff.declined)
	}
}

// Boundary inputs: extreme prices and ticks, minimum and maximum liquidity and
// amounts, and values one past each fixed-width limit.
func TestSwapV3ExactIn256MatchesBigOnBoundaries(t *testing.T) {
	type poolCase struct {
		name  string
		price *big.Int
		tick  int32
	}
	pools := []poolCase{
		{"min price", new(big.Int).Set(minSqrtRatioX96), MinTick},
		{"min price plus one", new(big.Int).Add(minSqrtRatioX96, one), MinTick},
		{"min tick plus one", mustTickPrice(MinTick + 1), MinTick + 1},
		{"max price minus one", new(big.Int).Sub(maxSqrtRatioX96, one), MaxTick - 1},
		{"max tick minus one", mustTickPrice(MaxTick - 1), MaxTick - 1},
		{"price one", new(big.Int).Set(Q96), 0},
		{"just below price one", new(big.Int).Sub(Q96, one), -1},
	}
	liquidities := []*big.Int{big.NewInt(1), big.NewInt(1_000_000), pow2Minus1(64), pow2Minus1(127), pow2Minus1(128), pow2(128), pow2(200)}
	amounts := []*big.Int{big.NewInt(1), big.NewInt(999), big.NewInt(1_000_000), pow2Minus1(64), pow2Minus1(128), pow2Minus1(160), pow2Minus1(255), pow2Minus1(256), pow2(256), pow2(300)}
	fees := []uint32{0, 1, 3000, 999999}
	var diff swapDifferential
	for _, pool := range pools {
		for _, liquidity := range liquidities {
			for _, fee := range fees {
				state := V3State{
					SqrtPriceX96: pool.price,
					Tick:         pool.tick,
					Liquidity:    liquidity,
					FeePips:      fee,
					TickSpacing:  60,
					Bitmap:       map[int16]*big.Int{},
					Ticks:        map[int32]TickInfo{},
				}
				for _, amount := range amounts {
					for _, zeroForOne := range []bool{true, false} {
						label := fmt.Sprintf("%s liquidity %s fee %d amount %s zeroForOne=%v", pool.name, liquidity, fee, amount, zeroForOne)
						diff.compare(t, label, state, zeroForOne, amount, nil, liquidity.BitLen() <= 128 && amount.BitLen() <= 256)
					}
				}
			}
		}
	}
	t.Log(&diff)
	if diff.handled == 0 || diff.declined == 0 {
		t.Fatalf("handled %d, declined %d: boundaries no longer reach both paths", diff.handled, diff.declined)
	}
}

// Crossing nets that push liquidity to and past 2^128 must leave the fixed
// path exactly when arbitrary precision would go further.
func TestSwapV3ExactIn256LiquidityAtWidthLimit(t *testing.T) {
	var diff swapDifferential
	for _, net := range []*big.Int{
		big.NewInt(1),
		new(big.Int).Sub(pow2Minus1(128), big.NewInt(1_000_000)),
		new(big.Int).Sub(pow2(128), big.NewInt(1_000_000)),
		pow2(128),
		new(big.Int).Neg(big.NewInt(1_000_000)),
		new(big.Int).Neg(big.NewInt(1_000_001)),
		new(big.Int).Neg(pow2(128)),
	} {
		for _, zeroForOne := range []bool{true, false} {
			state := crossingState()
			state.Liquidity = big.NewInt(1_000_000)
			crossTick := int32(10)
			signed := new(big.Int).Set(net)
			if zeroForOne {
				crossTick = -10
				signed.Neg(signed)
			}
			state.Bitmap = map[int16]*big.Int{}
			state.Ticks = map[int32]TickInfo{}
			addTickNet(&state, crossTick, signed)
			crossed := new(big.Int).Add(state.Liquidity, net)
			for _, amount := range []*big.Int{big.NewInt(1_000_000), pow2(100), pow2Minus1(255)} {
				label := fmt.Sprintf("net %s zeroForOne=%v amount %s", net, zeroForOne, amount)
				diff.compare(t, label, state, zeroForOne, amount, nil, crossed.BitLen() <= 128)
			}
		}
	}
	t.Log(&diff)
}

func TestSwapV3ExactIn256StepMatchesReference(t *testing.T) {
	random := rand.New(rand.NewSource(1559))
	var step swapStep256
	compared := 0
	for i := range 200_000 {
		tick := randomTick(random)
		targetTick := max(MinTick, min(MaxTick, tick+int32(random.Intn(40001)-20000)))
		current := mustTickPrice(tick)
		if random.Intn(2) == 0 && tick < MaxTick {
			current.Add(current, new(big.Int).Rand(random, new(big.Int).Sub(mustTickPrice(tick+1), current)))
		}
		target := mustTickPrice(targetTick)
		liquidity := randomLiquidity(random)
		if i%97 == 0 {
			liquidity = new(big.Int)
		}
		amount := randomAmount(random)
		fee := randomFee(random)

		// The fixed step requires liquidity below 2^128 and amounts below
		// 2^256; the swap declines anything wider before stepping.
		if liquidity.BitLen() > 128 || amount.BitLen() > 256 {
			continue
		}
		want, wantErr := referenceSwapStep(current, target, liquidity, amount, fee)
		var current256, target256, liquidity256, amount256 uint256.Int
		current256.SetFromBig(current)
		target256.SetFromBig(target)
		liquidity256.SetFromBig(liquidity)
		amount256.SetFromBig(amount)
		if !step.compute(&current256, &target256, &liquidity256, &amount256, fee) {
			// Declining is only allowed where the reference fails or a
			// value leaves the protocol's widths.
			if wantErr == nil && want.fee.Sign() >= 0 && want.sqrtPriceNext.Cmp(uint160Max) <= 0 {
				t.Fatalf("sample %d declined: tick %d->%d liquidity %s amount %s fee %d", i, tick, targetTick, liquidity, amount, fee)
			}
			continue
		}
		if wantErr != nil {
			t.Fatalf("sample %d: reference error %v, fixed step succeeded", i, wantErr)
		}
		for field, pair := range [][2]*big.Int{
			{step.next.ToBig(), want.sqrtPriceNext}, {step.amountIn.ToBig(), want.amountIn},
			{step.amountOut.ToBig(), want.amountOut}, {step.fee.ToBig(), want.fee},
		} {
			if pair[0].Cmp(pair[1]) != 0 {
				t.Fatalf("sample %d field %d tick %d->%d liquidity %s amount %s fee %d: got %s want %s",
					i, field, tick, targetTick, liquidity, amount, fee, pair[0], pair[1])
			}
		}
		compared++
	}
	if compared < 150_000 {
		t.Fatalf("only %d steps compared", compared)
	}
}
