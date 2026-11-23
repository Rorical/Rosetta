package remote

import (
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// Two consecutive sends: the first moves a pool one frame before the second
// lands, and the second was quoted on a view without the first. A route
// through the first send's pools is changed until the view used to quote
// reaches the position it landed at.
func TestOwnSendsMoveTheirPoolsUntilTheQuoteIncludesThem(t *testing.T) {
	var f ownFlight
	shared, other := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	sent := time.Unix(1_800_000_000, 0)
	f.sent(0, 94, []domain.PoolKey{shared}, 0, sent)
	quote := domain.StateVersion{Source: domain.SourceFast, BlockNumber: 100, TxIndex: 5}

	// Sent, not yet in the feed: the quote cannot include it.
	if moved := f.moved(quote, nil, 0, sent.Add(150*time.Millisecond)); !slices.Contains(moved, shared) || slices.Contains(moved, other) {
		t.Fatalf("unseen send: moved %v", moved)
	}
	// A replacement of 94 executes instead of it, so 94's effects do not matter.
	if moved := f.moved(quote, &execution.PendingTransaction{Nonce: 94}, 0, sent.Add(150*time.Millisecond)); len(moved) != 0 {
		t.Fatalf("replacing 94: moved %v", moved)
	}
	// Landed at index 7, after the quote's index 5: still not in the view.
	f.seen(0, 94, 100, 7)
	if moved := f.moved(quote, nil, 0, sent.Add(time.Second)); !slices.Contains(moved, shared) {
		t.Fatalf("landed after the quote: moved %v", moved)
	}
	// A view at or past index 7, or in a later block, includes it.
	for _, view := range []domain.StateVersion{{Source: domain.SourceFast, BlockNumber: 100, TxIndex: 7}, {Source: domain.SourceFast, BlockNumber: 101}, {Source: domain.SourceStandard, BlockNumber: 100}} {
		if moved := f.moved(view, nil, 0, sent.Add(time.Second)); len(moved) != 0 {
			t.Fatalf("view %+v includes 94: moved %v", view, moved)
		}
	}
	// A send the feed never shows stops counting after ownUnseen.
	f.sent(0, 96, []domain.PoolKey{other}, 0, sent)
	if moved := f.moved(quote, nil, 0, sent.Add(ownUnseen)); slices.Contains(moved, other) {
		t.Fatalf("stale unseen send: moved %v", moved)
	}
}

// The live path: after the send of the first nonce lands in a frame newer
// than the quote, the same quote is not built again for the next nonce.
func TestReactiveSkipsRoutesThroughOurLandedSendBeyondTheQuote(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	s.observations = make(chan any, 4)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	hash, ok := s.executeJob(t.Context(), job, s.primary())["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) {
		t.Fatal("not sent")
	}
	s.finishJob(job)
	// The quote's view is block 0; the send lands in block 10.
	r.observeReactiveExecution(hash, true, nil, execution.ExecutionPosition{Stream: "flashblocks", BlockNumber: 10, TransactionIndex: 4, ReceivedAtNS: time.Now().UnixNano()})
	if !s.dispatcher.Included() {
		t.Fatal("send not released")
	}
	s.takeSkips()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if next := s.takeJob(s.primary()); next != nil {
		t.Fatal("a route through our landed send was built on a view without it")
	}
	if skips := s.takeSkips(); skips["own_tx_not_in_quote"] == 0 {
		t.Fatalf("skips %v", skips)
	}
}

// Several sends through a transfer-taxed pair revert within one second.
// Nobody else has changed the pools, so each new frame quotes the same
// opportunity again. A revert on pools unchanged since its quote holds
// them until another transaction changes them or ownRevertHold passes.
func TestUnexplainedRevertHoldsItsPoolsUntilTheyChange(t *testing.T) {
	var f ownFlight
	busy, taxed, quiet := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}, domain.PoolKey{Address: domain.Address{3}}
	now := time.Unix(1_800_000_000, 0)
	quote := domain.StateVersion{Source: domain.SourceFast, BlockNumber: 100, TxIndex: 5}
	f.changed([]domain.PoolKey{busy, taxed}, 99, 3)
	f.reverted([]domain.PoolKey{busy, taxed}, quote, 100, 8, now)
	if held := f.held(now); !slices.Contains(held, busy) || !slices.Contains(held, taxed) {
		t.Fatalf("unexplained revert: held %v", held)
	}
	// A rival swap on one pool releases that pool only.
	f.changed([]domain.PoolKey{busy}, 100, 9)
	if held := f.held(now); slices.Contains(held, busy) || !slices.Contains(held, taxed) {
		t.Fatalf("after busy changed: held %v", held)
	}
	if held := f.held(now.Add(ownRevertHold)); len(held) != 0 {
		t.Fatalf("hold outlived ownRevertHold: %v", held)
	}

	// A pool changed after the quote explains the revert: we lost the race.
	f = ownFlight{}
	f.changed([]domain.PoolKey{quiet}, 100, 7)
	f.reverted([]domain.PoolKey{busy, quiet}, quote, 100, 8, now)
	if held := f.held(now); len(held) != 0 {
		t.Fatalf("lost auction: held %v", held)
	}
}

// The live path of the opCEN case: our send reverts, the next view includes
// the revert, and the opportunity is still there because no pool changed.
// Without the hold the same route is built and sent again.
func TestReactiveHoldsTheRouteOfAnUnexplainedRevert(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	s.observations = make(chan any, 4)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	hash, ok := s.executeJob(t.Context(), job, s.primary())["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) {
		t.Fatal("not sent")
	}
	s.finishJob(job)
	version := event.Job.Version
	r.observeReactiveExecution(hash, false, nil, execution.ExecutionPosition{Stream: "flashblocks", BlockNumber: version.BlockNumber, TransactionIndex: version.TxIndex, ReceivedAtNS: time.Now().UnixNano()})
	s.takeSkips()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if next := s.takeJob(s.primary()); next != nil {
		t.Fatal("the reverted route was built again on unchanged pools")
	}
	if skips := s.takeSkips(); skips["own_revert_unexplained"] == 0 {
		t.Fatalf("skips %v", skips)
	}
}

// Lanes have their own nonces: lane 1's nonce 94 is not lane 0's.
func TestOwnFlightKeepsLanesApart(t *testing.T) {
	var f ownFlight
	a, b := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	now := time.Now()
	version := domain.StateVersion{BlockNumber: 100, Source: domain.SourceFast, TxIndex: 9}
	f.sent(0, 94, []domain.PoolKey{a}, 0, now)
	f.sent(1, 94, []domain.PoolKey{b}, 0, now)
	f.seen(0, 94, 100, 3)
	if moved := f.moved(version, nil, 0, now); len(moved) != 1 || moved[0] != b {
		t.Fatal("one lane's sighting released the other lane's nonce", moved)
	}
	if moved := f.moved(version, &execution.PendingTransaction{Nonce: 94}, 0, now); len(moved) != 1 || moved[0] != b {
		t.Fatal("a replacement on the primary lane left out another lane's nonce", moved)
	}
}

// A conditional send through a pool is rejected for free (the pool had
// moved), and a second later the same route is rebuilt from a view that does
// not show the move yet, sent without the pool's condition and reverts for
// about 48,000 gas. A rejected route's pools are
// held until the stream shows them changed, or for ownRejectHold.
func TestPoolsOfARejectedConditionalSendAreHeldUntilTheyChange(t *testing.T) {
	var f ownFlight
	pools := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}}
	now := time.Unix(1_800_000_000, 0)
	f.changed(pools[:1], 100, 3)
	f.rejected(pools, now)
	if held := f.held(now.Add(500 * time.Millisecond)); len(held) != 2 {
		t.Fatalf("held %v after a rejection", held)
	}
	// The move that caused the rejection arrives.
	f.changed(pools[:1], 100, 5)
	if held := f.held(now.Add(600 * time.Millisecond)); len(held) != 1 || held[0] != pools[1] {
		t.Fatalf("held %v after the pool changed", held)
	}
	// Nothing else shows up: the hold ends on its own.
	if held := f.held(now.Add(ownRejectHold)); len(held) != 0 {
		t.Fatalf("held %v past the hold", held)
	}
	// A revert's hold still lasts ownRevertHold.
	f.reverted(pools, domain.StateVersion{BlockNumber: 100, TxIndex: 5}, 100, 6, now)
	if held := f.held(now.Add(ownRejectHold)); len(held) == 0 {
		t.Fatal("a revert's hold ended as early as a rejection's")
	}
}
