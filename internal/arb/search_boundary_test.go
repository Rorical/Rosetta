package arb

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Under bursty Fast Feed load about half of started searches are canceled,
// nearly all at a lineage boundary and very few because newer frames changed
// every pool they ranked through. The boundary records which case applied.
func TestLaneCancelAtRecordsWhatTheBoundaryFrameChanged(t *testing.T) {
	pool := func(b byte) domain.PoolKey {
		return domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{b}}
	}
	canceled := 0
	run := func(coarse bool, triggers ...domain.PoolKey) *searchRun {
		r := &searchRun{cancel: func() { canceled++ }, triggers: triggers}
		if coarse {
			r.coarse = func() {}
		}
		return r
	}
	untouched, touched, sizing := run(true, pool(1), pool(2)), run(true, pool(3)), run(false)
	lane := &laneRuns{alive: []*searchRun{untouched, touched, sizing}}
	lane.cancelAt(BoundaryBlock, []domain.PoolKey{pool(3), pool(9)})
	for name, c := range map[string]struct {
		run  *searchRun
		want BoundaryReach
	}{"untouched": {untouched, ReachUntouched}, "touched": {touched, ReachTouched}, "sizing": {sizing, ReachSizing}} {
		if c.run.boundary != (SearchBoundary{Kind: BoundaryBlock, Reached: c.want}) {
			t.Fatalf("%s: %+v", name, c.run.boundary)
		}
	}
	if canceled != 3 {
		t.Fatalf("canceled %d runs", canceled)
	}
	// A later boundary does not relabel a run that is already ending.
	lane.cancelAt(BoundaryEpoch, []domain.PoolKey{pool(1)})
	if untouched.boundary != (SearchBoundary{Kind: BoundaryBlock, Reached: ReachUntouched}) {
		t.Fatalf("relabelled: %+v", untouched.boundary)
	}

	base := domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10, Anchor: domain.BlockRef{Number: 9}}
	next := base
	next.BlockNumber = 11
	if boundaryKind(base, next) != BoundaryBlock {
		t.Fatal("a new block is a block boundary")
	}
	next = base
	next.Anchor.Number = 10
	if boundaryKind(base, next) != BoundaryAnchor {
		t.Fatal("a new anchor at the same block is an anchor boundary")
	}
	next.Epoch = 2
	if boundaryKind(base, next) != BoundaryEpoch {
		t.Fatal("a new epoch is an epoch boundary")
	}
}
