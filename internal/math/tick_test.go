package math

import (
	"errors"
	"math/big"
	"testing"
)

func TestTickToSqrtPriceKnownVectors(t *testing.T) {
	tests := []struct {
		name string
		tick int32
		want string
	}{
		{"zero", 0, "79228162514264337593543950336"},
		{"minimum", MinTick, "4295128739"},
		{"maximum", MaxTick, "1461446703485210103287273052203988822378723970342"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := TickToSqrtPriceX96(test.tick)
			if err != nil {
				t.Fatalf("TickToSqrtPriceX96(%d): %v", test.tick, err)
			}
			if want := bigFromString(t, test.want); got.Cmp(want) != 0 {
				t.Fatalf("tick %d = %s, want %s", test.tick, got, want)
			}
		})
	}
}

func TestTickZeroIsQ96(t *testing.T) {
	got, err := TickToSqrtPriceX96(0)
	if err != nil {
		t.Fatalf("TickToSqrtPriceX96(0): %v", err)
	}
	want := new(big.Int).Lsh(big.NewInt(1), Q96Bits)
	if got.Cmp(want) != 0 {
		t.Fatalf("tick 0 = %s, want 2^96 = %s", got, want)
	}
	if got.Cmp(Q96) != 0 {
		t.Fatalf("tick 0 = %s, want Q96 = %s", got, Q96)
	}
}

func TestTickRoundTrip(t *testing.T) {
	ticks := []int32{MinTick, -1000, -1, 0, 1, 1000, MaxTick}
	for _, tick := range ticks {
		t.Run(big.NewInt(int64(tick)).String(), func(t *testing.T) {
			price := sqrtAt(t, tick)
			got, err := SqrtPriceX96ToTick(price)
			if err != nil {
				t.Fatalf("SqrtPriceX96ToTick(%s): %v", price, err)
			}
			if got != tick {
				t.Fatalf("round trip tick %d -> %s -> %d", tick, price, got)
			}
			if tick > MinTick {
				below := new(big.Int).Sub(price, big.NewInt(1))
				got, err = SqrtPriceX96ToTick(below)
				if err != nil {
					t.Fatalf("SqrtPriceX96ToTick(price-1): %v", err)
				}
				if got != tick-1 {
					t.Fatalf("price-1 for tick %d returned %d, want %d", tick, got, tick-1)
				}
			}
		})
	}
}

func TestTickPriceInsideInterval(t *testing.T) {
	for tick := int32(-1000); tick <= 1000; tick += 37 {
		lower := sqrtAt(t, tick)
		upper := sqrtAt(t, tick+1)
		midpoint := new(big.Int).Rsh(new(big.Int).Add(lower, upper), 1)
		got, err := SqrtPriceX96ToTick(midpoint)
		if err != nil {
			t.Fatalf("tick %d midpoint: %v", tick, err)
		}
		if got != tick {
			t.Fatalf("tick %d midpoint returned %d", tick, got)
		}
	}
}

func TestSqrtPriceToTickMatchesBinarySearch(t *testing.T) {
	prices := []*big.Int{
		sqrtAt(t, MinTick),
		new(big.Int).Sub(sqrtAt(t, MaxTick), big.NewInt(1)),
		new(big.Int).Sub(Q96, big.NewInt(1)),
		new(big.Int).Set(Q96),
		new(big.Int).Add(Q96, big.NewInt(1)),
	}
	for tick := int32(-8000); tick <= 8000; tick += 211 {
		lower := sqrtAt(t, tick)
		upper := sqrtAt(t, tick+1)
		prices = append(prices, new(big.Int).Rsh(new(big.Int).Add(lower, upper), 1))
	}

	for _, price := range prices {
		got, err := SqrtPriceX96ToTick(price)
		if err != nil {
			t.Fatalf("SqrtPriceX96ToTick(%s): %v", price, err)
		}
		want := tickByBinarySearch(t, price)
		if got != want {
			t.Fatalf("price %s returned tick %d, binary search %d", price, got, want)
		}
	}
}

func TestTickOutOfRange(t *testing.T) {
	for _, tick := range []int32{MinTick - 1, MaxTick + 1} {
		if _, err := TickToSqrtPriceX96(tick); !errors.Is(err, ErrTickOutOfRange) {
			t.Fatalf("TickToSqrtPriceX96(%d) error = %v, want ErrTickOutOfRange", tick, err)
		}
	}
}

func TestSqrtPriceOutOfRange(t *testing.T) {
	tests := []struct {
		name  string
		price *big.Int
	}{
		{"nil", nil},
		{"zero", new(big.Int)},
		{"below minimum", new(big.Int).Sub(minSqrtRatioX96, big.NewInt(1))},
		{"above maximum", new(big.Int).Add(maxSqrtRatioX96, big.NewInt(1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SqrtPriceX96ToTick(test.price); err == nil {
				t.Fatalf("SqrtPriceX96ToTick(%v) returned no error", test.price)
			}
		})
	}
}

func TestSqrtPriceMaximumIsMaxTick(t *testing.T) {
	got, err := SqrtPriceX96ToTick(new(big.Int).Set(maxSqrtRatioX96))
	if err != nil {
		t.Fatalf("SqrtPriceX96ToTick(maximum): %v", err)
	}
	if got != MaxTick {
		t.Fatalf("maximum price tick = %d, want %d", got, MaxTick)
	}
}

func tickByBinarySearch(t *testing.T, price *big.Int) int32 {
	t.Helper()
	low, high := MinTick, MaxTick
	for low < high {
		mid := low + (high-low+1)/2
		if sqrtAt(t, mid).Cmp(price) <= 0 {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low
}

func TestTickBitmapNextInitialized(t *testing.T) {
	bitmap := map[int16]*big.Int{
		-1: wordWithBits(255, 254, 252),
		0:  wordWithBits(1, 2, 4),
	}
	tests := []struct {
		name     string
		tick     int32
		spacing  int32
		lte      bool
		wantTick int32
		wantInit bool
	}{
		{"lte on initialized", 20, 10, true, 20, true},
		{"lte between ticks", 25, 10, true, 20, true},
		{"lte below first", 9, 10, true, 0, false},
		{"gt from zero", 0, 10, false, 10, true},
		{"gt between ticks", 15, 10, false, 20, true},
		{"gt above last", 40, 10, false, 2550, false},
		{"lte negative word", -1, 10, true, -10, true},
		{"lte finds lower bit", -21, 10, true, -40, true},
		{"lte below negative word", -41, 10, true, -2560, false},
		{"gt negative word", -20, 10, false, -10, true},
		{"zero spacing", 10, 0, true, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tick, initialized := TickBitmapNextInitialized(bitmap, test.tick, test.spacing, test.lte)
			if tick != test.wantTick || initialized != test.wantInit {
				t.Fatalf("got (%d, %v), want (%d, %v)", tick, initialized, test.wantTick, test.wantInit)
			}
		})
	}
}
