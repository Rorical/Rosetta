package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// The oldest arrival leaves when the ring is full; a repeated message keeps
// its first arrival.
func TestFeedArrivalsKeepFirstAndEvictOldest(t *testing.T) {
	f := newFeedArrivals()
	start := time.Unix(1000, 0)
	hash := func(i int) domain.Hash { return domain.Hash{byte(i), byte(i >> 8)} }
	for i := range feedArrivalCap {
		f.observe(hash(i), uint64(i), start.Add(time.Duration(i)*time.Millisecond))
	}
	f.observe(hash(1), 1, start.Add(time.Hour))
	if at, ok := f.arrival(hash(1)); !ok || !at.Equal(start.Add(time.Millisecond)) {
		t.Fatal("repeat replaced the first arrival", at)
	}
	f.observe(hash(feedArrivalCap), feedArrivalCap, start)
	if _, ok := f.arrival(hash(0)); ok {
		t.Fatal("oldest arrival kept past the bound")
	}
	if _, ok := f.arrival(hash(feedArrivalCap)); !ok || len(f.byHash) != feedArrivalCap || len(f.bySeq) != feedArrivalCap {
		t.Fatal("bound", len(f.byHash), len(f.bySeq))
	}
	if _, ok := (*feedArrivals)(nil).arrival(hash(1)); ok {
		t.Fatal("nil tracker reported an arrival")
	}
}

// A Fast message knows only its block number. Once a canonical block matches
// a feed hash, block = sequence + offset finds the arrival of any block.
func TestFeedArrivalsByBlockNumber(t *testing.T) {
	f := newFeedArrivals()
	at := time.Unix(2000, 0)
	f.observe(domain.Hash{1}, 500, at)
	f.observe(domain.Hash{2}, 501, at.Add(100*time.Millisecond))
	if _, ok := f.arrivalAt(1_000_501); ok {
		t.Fatal("arrival by number before the offset is known")
	}
	f.learn(domain.Hash{1}, 1_000_500)
	if got, ok := f.arrivalAt(1_000_501); !ok || !got.Equal(at.Add(100*time.Millisecond)) {
		t.Fatal("arrival by number", got, ok)
	}
	if _, ok := f.arrivalAt(1_000_502); ok {
		t.Fatal("arrival for a block the feed never sent")
	}
}
