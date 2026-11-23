package pool

import "github.com/Rorical/Rosetta/internal/math"

// maxWindowRadius is the largest bitmap word radius the deep loaders accept.
const maxWindowRadius = 64

// WindowRadius returns the bitmap word radius a deep load uses for a pool with
// tickSpacing. Words of radius r cover at least r*256*tickSpacing ticks on both
// sides of any tick in the current word, so the result covers
// math.CoverageTicks: 9 words at spacing 1, 5 at 2, 2 at 5 and 1 from 9 up.
// floor is the configured search radius and is never reduced.
func WindowRadius(tickSpacing int32, floor int16) int16 {
	if tickSpacing <= 0 {
		return floor
	}
	width := int64(tickSpacing) * 256
	radius := max(int64(floor), (int64(math.CoverageTicks)+width-1)/width)
	return int16(min(maxWindowRadius, radius))
}

// WindowCovers reports whether a loaded window reaches ticks on both sides of
// tick. A side clamped to the tick range bound is complete.
func WindowCovers(tick, windowMin, windowMax int32, ticks int64) bool {
	if windowMin == 0 && windowMax == 0 || tick < windowMin || tick > windowMax {
		return false
	}
	below := windowMin == math.MinTick || int64(tick)-int64(windowMin) >= ticks
	above := windowMax == math.MaxTick || int64(windowMax)-int64(tick) >= ticks
	return below && above
}
