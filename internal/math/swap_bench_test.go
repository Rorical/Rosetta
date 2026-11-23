package math

import (
	"math/big"
	"testing"
)

// denseState has an initialized tick every spacing over [-count, count]
// spacings, each adding liquidity when crossed downward, so a zero-for-one
// swap crosses many ticks with liquidity changing at each.
func denseState(spacing int32, count int) V3State {
	base := big.NewInt(1_000_000_000_000_000_000)
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Liquidity:    new(big.Int).Set(base),
		TickSpacing:  spacing,
		Bitmap:       make(map[int16]*big.Int),
		Ticks:        make(map[int32]TickInfo),
	}
	for i := -count; i <= count; i++ {
		if i == 0 {
			continue
		}
		tick := int32(i) * spacing
		compressed := floorDiv(tick, spacing)
		word := int16(compressed >> 8)
		if state.Bitmap[word] == nil {
			state.Bitmap[word] = new(big.Int)
		}
		state.Bitmap[word].SetBit(state.Bitmap[word], int(compressed&255), 1)
		net := new(big.Int).Div(base, big.NewInt(int64(10+i%7)))
		if i < 0 {
			net.Neg(net)
		}
		state.Ticks[tick] = TickInfo{LiquidityGross: new(big.Int).Abs(net), LiquidityNet: net}
	}
	return state
}

func BenchmarkSwapV3ExactIn(b *testing.B) {
	dense := denseState(10, 400)
	denseAmount := big.NewInt(400_000_000_000_000_000)
	cases := []struct {
		name       string
		state      V3State
		zeroForOne bool
		amount     *big.Int
		crossed    int
		coldTicks  bool
	}{
		{"no-cross", crossingState(), true, big.NewInt(1_000_000_000_000), 0, false},
		{"cross-3", crossingState(), true, big.NewInt(2_000_000_000_000_000), 2, false},
		{"cross-dense-down", dense, true, denseAmount, -1, false},
		{"cross-dense-up", dense, false, denseAmount, -1, false},
		// Production quotes span many pools and mostly miss the shared tick
		// price cache.
		{"cross-dense-down-cold", dense, true, denseAmount, -1, true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			result, err := SwapV3ExactIn(tc.state, tc.zeroForOne, tc.amount)
			if err != nil {
				b.Fatal(err)
			}
			if tc.crossed >= 0 && len(result.Crossed) != tc.crossed {
				b.Fatalf("crossed %d ticks, want %d", len(result.Crossed), tc.crossed)
			}
			b.ReportAllocs()
			for b.Loop() {
				if tc.coldTicks {
					b.StopTimer()
					for i := range tickPrices {
						tickPrices[i].Store(nil)
					}
					b.StartTimer()
				}
				if _, err := SwapV3ExactIn(tc.state, tc.zeroForOne, tc.amount); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Production quotes span many pools, so most tick prices miss the shared
// cache; this measures the uncached tick-to-price conversion.
func BenchmarkTickToSqrtPriceUncached(b *testing.B) {
	b.ReportAllocs()
	tick := MinTick
	for b.Loop() {
		if _, err := calculateTickPrice(tick); err != nil {
			b.Fatal(err)
		}
		tick += 7919
		if tick > MaxTick {
			tick = MinTick + (tick - MaxTick)
		}
	}
}

func BenchmarkSqrtPriceX96ToTick(b *testing.B) {
	prices := make([]*big.Int, 0, 64)
	for tick := MinTick; tick < MaxTick; tick += 27733 {
		price, err := TickToSqrtPriceX96(tick)
		if err != nil {
			b.Fatal(err)
		}
		prices = append(prices, price.Add(price, big.NewInt(12345)))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if _, err := SqrtPriceX96ToTick(prices[i%len(prices)]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}
