package pool

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/math"
)

func TestWindowRadiusCoversTwentyPercent(t *testing.T) {
	for _, test := range []struct {
		spacing int32
		floor   int16
		want    int16
	}{
		{1, 1, 9}, {2, 1, 5}, {5, 1, 2}, {8, 1, 2}, {9, 1, 1}, {10, 1, 1}, {60, 1, 1}, {200, 1, 1},
		{1, 12, 12}, {10, 3, 3}, {0, 2, 2},
	} {
		got := WindowRadius(test.spacing, test.floor)
		if got != test.want {
			t.Fatalf("spacing %d floor %d: radius %d, want %d", test.spacing, test.floor, got, test.want)
		}
		// The first tick of the current word is the worst case below; the last is above.
		if test.spacing > 0 && int64(got)*256*int64(test.spacing) < math.CoverageTicks {
			t.Fatalf("spacing %d radius %d covers less than %d ticks", test.spacing, got, math.CoverageTicks)
		}
	}
}

func TestWindowCovers(t *testing.T) {
	for _, test := range []struct {
		name            string
		tick, low, high int32
		ticks           int64
		want            bool
	}{
		{"both sides", 0, -2304, 2559, 2232, true},
		{"short below", 0, -2231, 2559, 2232, false},
		{"short above", 0, -2304, 2231, 2232, false},
		{"clamped bound", 887200, 860000, math.MaxTick, 2232, true},
		{"unknown", 5, 0, 0, 1, false},
		{"tick outside", 9000, -2304, 2559, 1, false},
	} {
		if got := WindowCovers(test.tick, test.low, test.high, test.ticks); got != test.want {
			t.Fatalf("%s: got %v", test.name, got)
		}
	}
}
