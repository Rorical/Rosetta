package remote

import (
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// feedArrivalCap bounds the remembered standard feed messages: about 400 s of
// Robinhood blocks, far longer than a block waits to be applied.
const feedArrivalCap = 4096

// feedArrivals remembers when each standard feed message arrived, by block
// hash and by sequence. In RPC mode the feed only wakes the follower; the
// block is applied once the local node serves it, so its arrival is the
// earliest moment the block was known. A canonical block that matches a feed
// hash fixes block = sequence + offset, which lets a Fast message, which
// carries only a block number, find its feed arrival. The feed goroutine
// writes, the runtime reads.
type feedArrivals struct {
	mu     sync.Mutex
	byHash map[domain.Hash]feedArrival
	bySeq  map[uint64]time.Time
	order  []feedArrival // ring of insertion order for eviction
	next   int
	offset int64
	known  bool
}

type feedArrival struct {
	hash domain.Hash
	seq  uint64
	at   time.Time
}

func newFeedArrivals() *feedArrivals {
	return &feedArrivals{byHash: make(map[domain.Hash]feedArrival, feedArrivalCap), bySeq: make(map[uint64]time.Time, feedArrivalCap), order: make([]feedArrival, feedArrivalCap)}
}

func (f *feedArrivals) observe(hash domain.Hash, seq uint64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byHash[hash]; ok {
		return
	}
	if old := f.order[f.next]; !old.at.IsZero() {
		delete(f.byHash, old.hash)
		delete(f.bySeq, old.seq)
	}
	entry := feedArrival{hash: hash, seq: seq, at: at}
	f.order[f.next] = entry
	f.next = (f.next + 1) % feedArrivalCap
	f.byHash[hash] = entry
	f.bySeq[seq] = at
}

func (f *feedArrivals) arrival(hash domain.Hash) (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.byHash[hash]
	return entry.at, ok
}

// learn ties a canonical block to the feed message that carried it.
func (f *feedArrivals) learn(hash domain.Hash, number uint64) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.byHash[hash]; ok {
		f.offset, f.known = int64(number)-int64(entry.seq), true
	}
}

// arrivalAt returns the feed arrival of block number, once learn has fixed
// the sequence offset.
func (f *feedArrivals) arrivalAt(number uint64) (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known || int64(number) < f.offset {
		return time.Time{}, false
	}
	at, ok := f.bySeq[uint64(int64(number)-f.offset)]
	return at, ok
}

// sequenceOffset is block - sequence, once learn has fixed it.
func (f *feedArrivals) sequenceOffset() (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offset, f.known
}
