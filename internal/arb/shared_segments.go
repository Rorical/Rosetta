package arb

import (
	"github.com/Rorical/Rosetta/internal/math"
)

const localSegmentCount = 128

// A shard owns this tiny front cache. Shared numerical entries remain in the
// bounded striped cache; repeated local segments need no mutex acquisition.
type localSegments struct {
	entries [localSegmentCount]segmentEntry
}

func (m *localSegments) reset() {
	for i := range m.entries {
		m.entries[i].valid = false
	}
}
func (m *localSegments) load(key segmentKey, curve *math.CycleCoefficients) bool {
	entry := &m.entries[segmentSlot(key)%localSegmentCount]
	if !entry.valid || entry.key != key {
		return false
	}
	curve.Set(&entry.curve)
	return true
}
func (m *localSegments) store(key segmentKey, curve *math.CycleCoefficients) {
	entry := &m.entries[segmentSlot(key)%localSegmentCount]
	entry.key = key
	entry.valid = true
	entry.curve.Set(curve)
}

func (w *rankWorkspace) prepareSegment(s *compiledSnapshot, hops []uint32, offset, length int, cache *rankCache) {
	key := segmentKey{length: length}
	for i := range length {
		hop := hops[(offset+i)%len(hops)]
		key.hops[i] = hop
		if cache != nil {
			key.revisions[i] = cache.pools[hop/2].revision
		}
	}
	curve := &w.segmentCurve.Curve
	if w.localSegments.load(key, curve) {
		return
	}
	if cache != nil && cache.segments.load(key, curve) {
		w.localSegments.store(key, curve)
		return
	}
	hop := hops[offset%len(hops)]
	curve.Set(&s.pools[hop/2].legs[hop%2].curve)
	for i := 1; i < length; i++ {
		hop := hops[(offset+i)%len(hops)]
		w.segmentCurve.Append(&s.pools[hop/2].legs[hop%2].curve)
	}
	if cache != nil {
		cache.segments.store(key, curve)
	}
	w.localSegments.store(key, curve)
}
