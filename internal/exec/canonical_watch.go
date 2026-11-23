package exec

import (
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// CanonicalReceipt is one of our transactions in a canonical block, taken from
// the block receipts the canonical follower already reads.
type CanonicalReceipt struct {
	Hash    domain.Hash
	Success bool
	Logs    []rpc.Log
	Fees    *domain.ReceiptFees
}

type sighting struct {
	block   domain.BlockRef
	receipt CanonicalReceipt
}

// maxSightings bounds receipts waiting for Watch; each names a variant of
// at most maxIncluded+1 unfinished nonces, so it is never reached in practice.
const maxSightings = 64

// canonicalView is what the canonical follower reported: our receipts since the
// last Watch, and the block range it has read without a gap.
type canonicalView struct {
	mu        sync.Mutex
	from, to  uint64 // to == 0: nothing read since start or the last gap
	sightings []sighting
}

// ObserveCanonical records a canonical block for Watch. ours holds only
// receipts of watched hashes. It never waits for the send lock.
func (d *Dispatcher) ObserveCanonical(block domain.BlockRef, ours []CanonicalReceipt) {
	c := &d.canonical
	c.mu.Lock()
	if c.to == 0 || block.Number != c.to+1 {
		c.from = block.Number
	}
	c.to = block.Number
	for _, receipt := range ours {
		c.sightings = append(c.sightings, sighting{block, receipt})
	}
	if len(c.sightings) > maxSightings {
		c.sightings = c.sightings[len(c.sightings)-maxSightings:]
	}
	c.mu.Unlock()
	hashes := make([]domain.Hash, len(ours))
	for i, receipt := range ours {
		hashes[i] = receipt.Hash
	}
	d.ObserveIncluded(hashes)
}

// ObserveCanonicalGap records that the follower moved to a new head without
// reading the blocks in between, so their absence proves nothing.
func (d *Dispatcher) ObserveCanonicalGap() {
	c := &d.canonical
	c.mu.Lock()
	c.from, c.to = 0, 0
	c.mu.Unlock()
}

func (c *canonicalView) take() ([]sighting, uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.sightings
	c.sightings = nil
	return taken, c.from, c.to
}
