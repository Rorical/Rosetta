package math

import (
	"errors"
	"github.com/holiman/uint256"
	"sync"
	"testing"
)

func TestTickCacheCollisionMutationAndConcurrency(t *testing.T) {
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := range 400 {
				tick := int32(-800000 + worker*4096 + i*4096) // Regular strides that collided under the old modulo index.
				if tick > MaxTick {
					tick = MaxTick
				}
				want, err := calculateTickPrice(tick)
				if err != nil {
					t.Error(err)
					return
				}
				got, err := TickToSqrtPriceX96(tick)
				if err != nil || got.Cmp(want) != 0 {
					t.Errorf("tick %d: %v", tick, err)
					return
				}
				got.SetInt64(0)
				again, err := TickToSqrtPriceX96(tick)
				if err != nil || again.Cmp(want) != 0 {
					t.Errorf("mutated cache at tick %d", tick)
					return
				}
			}
		}(worker)
	}
	workers.Wait()
	for _, tick := range []int32{MinTick - 1, MaxTick + 1} {
		if _, err := TickToSqrtPriceX96(tick); !errors.Is(err, ErrTickOutOfRange) {
			t.Fatal(tick, err)
		}
	}
}

// The cache returns what the calculation returns, on a miss, on a hit and
// after another tick took the slot.
func TestCachedSqrtRatioMatchesCalculation(t *testing.T) {
	ticks := []int32{MinTick, -887220, -198765, -1, 0, 1, 60, 198765, MaxTick}
	// Ticks 65,536 apart share a slot when the multiplier's low bits agree.
	for i := range int32(64) {
		ticks = append(ticks, -400000+i*4096, -400000+i*4096+1)
	}
	for round := range 2 {
		for _, tick := range ticks {
			var got, want uint256.Int
			cachedSqrtRatioAtTick(&got, tick)
			sqrtRatioAtTick(&want, tick)
			if !got.Eq(&want) {
				t.Fatalf("round %d tick %d: cached %s, calculated %s", round, tick, &got, &want)
			}
		}
	}
}

func BenchmarkTickPriceCached(b *testing.B) {
	for b.Loop() {
		if _, err := tickPrice(-198765); err != nil {
			b.Fatal(err)
		}
	}
}

// Bitmap word boundaries must not continually evict each other.
func BenchmarkTickPriceStrided(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for i := range int32(32) {
			if _, err := tickPrice(-400000 + i*4096); err != nil {
				b.Fatal(err)
			}
		}
	}
}
