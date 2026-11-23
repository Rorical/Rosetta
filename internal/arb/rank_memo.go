package arb

import (
	"math/big"
	"sync"

	"github.com/Rorical/Rosetta/internal/math"
)

const estimateMemoSize = 32768
const segmentMemoSize = 16384
const memoStripes = 64

type estimateEntry struct {
	key                     estimateKey
	revision, funding, pass uint64
	input, net              big.Int
}

// Fixed slots cap memory independently of the path table. Collisions replace
// entries, never identify them: every lookup checks the complete key/revisions.
// Protect entries used in this or the previous pass from sequential-scan churn;
// a new dirty working set can replace them after one unused pass.
// Copy under the stripe lock so eviction cannot mutate a reader's integers.
type estimateMemo struct {
	locks [memoStripes]sync.Mutex
	slots []estimateEntry
}

func estimateSlot(key estimateKey) int {
	value := uint64(key.path)*0x9e3779b97f4a7c15 + uint64(key.rotation)*0xbf58476d1ce4e5b9
	value ^= value >> 32
	return int(value & (estimateMemoSize - 1))
}
func (m *estimateMemo) reset() {
	if m.slots == nil {
		m.slots = make([]estimateEntry, estimateMemoSize)
		return
	}
	for i := range m.slots {
		m.slots[i].revision = 0
	}
}
func (m *estimateMemo) load(key estimateKey, revision, funding, pass uint64, input, net *big.Int) bool {
	index := estimateSlot(key)
	lock := &m.locks[index%memoStripes]
	lock.Lock()
	defer lock.Unlock()
	entry := &m.slots[index]
	if entry.revision != revision || revision == 0 || entry.funding != funding || entry.key != key {
		return false
	}
	entry.pass = pass
	input.Set(&entry.input)
	net.Set(&entry.net)
	return true
}
func (m *estimateMemo) store(key estimateKey, revision, funding, pass uint64, input, net *big.Int) {
	index := estimateSlot(key)
	lock := &m.locks[index%memoStripes]
	lock.Lock()
	defer lock.Unlock()
	entry := &m.slots[index]
	if entry.revision != 0 && entry.key != key && pass-entry.pass <= 1 {
		return
	}
	entry.pass = pass
	entry.key = key
	entry.revision = revision
	entry.funding = funding
	entry.input.Set(input)
	entry.net.Set(net)
}

type segmentKey struct {
	hops      [3]uint32
	revisions [3]uint64
	length    int
}
type segmentEntry struct {
	key   segmentKey
	valid bool
	curve math.CycleCoefficients
}
type segmentMemo struct {
	locks [memoStripes]sync.Mutex
	slots []segmentEntry
}

func segmentSlot(key segmentKey) int {
	value := uint64(key.length)
	for i := 0; i < key.length; i++ {
		value = (value ^ uint64(key.hops[i])) * 0x9e3779b97f4a7c15
	}
	value ^= value >> 32
	return int(value & (segmentMemoSize - 1))
}
func (m *segmentMemo) reset() {
	if m.slots == nil {
		m.slots = make([]segmentEntry, segmentMemoSize)
		return
	}
	for i := range m.slots {
		m.slots[i].valid = false
	}
}
func (m *segmentMemo) load(key segmentKey, curve *math.CycleCoefficients) bool {
	index := segmentSlot(key)
	lock := &m.locks[index%memoStripes]
	lock.Lock()
	defer lock.Unlock()
	entry := &m.slots[index]
	if !entry.valid || entry.key != key {
		return false
	}
	curve.Set(&entry.curve)
	return true
}
func (m *segmentMemo) store(key segmentKey, curve *math.CycleCoefficients) {
	index := segmentSlot(key)
	lock := &m.locks[index%memoStripes]
	lock.Lock()
	defer lock.Unlock()
	entry := &m.slots[index]
	entry.key = key
	entry.valid = true
	entry.curve.Set(curve)
}
