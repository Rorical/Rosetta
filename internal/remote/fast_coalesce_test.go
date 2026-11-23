package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

// Frames queued behind one another are searched once, at the newest prefix,
// with every pool they changed and the earliest arrival: a quote of an older
// prefix is not current and would be dropped after delaying the newer frames.
func TestQueuedFastFramesShareOneSearch(t *testing.T) {
	store, pools := poolFixture(t)
	fast, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 8, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSearch{}
	r := &runtime{store: store, fast: fast, worker: &arb.Worker{}, topology: topology, search: pipeline.NewSearchScheduler(sink, 1, nil)}
	frame := func(index uint64, p int, reserve int64) feed.FastMessage {
		return feed.FastMessage{BlockNumber: 11, TxIndex: index, TxHash: domain.Hash{byte(index + 1)}, RawTx: []byte{byte(index + 1)},
			Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[p].Key, reserve)}}}
	}
	first := time.Now().Add(-time.Second)
	// Both frames reached the input queue before the main loop took the first.
	r.inputRevision.Store(2)
	if err := r.acceptFastMessage(t.Context(), frame(0, 0, 3_000_000), first, 1); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 0 {
		t.Fatal("a superseded prefix was searched")
	}
	if err := r.acceptFastMessage(t.Context(), frame(1, 1, 2_000_000), first.Add(time.Millisecond), 2); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 {
		t.Fatal("newest prefix searched", len(sink.jobs), "times")
	}
	job := sink.jobs[0]
	if len(job.Dirty) != 2 || !job.ReceivedAt.Equal(first) || job.Version != r.tentative {
		t.Fatal("search lost a queued frame's pools or its arrival", job.Dirty, job.ReceivedAt, first)
	}
	// Nothing is queued now: the next frame searches on its own.
	r.inputRevision.Store(3)
	if err := r.acceptFastMessage(t.Context(), frame(2, 0, 4_000_000), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 2 || len(sink.jobs[1].Dirty) != 1 {
		t.Fatal("deferred pools leaked into a later search", len(sink.jobs))
	}
}

// A reconciled canonical block's pools go to the full search only. The
// fast lane quotes only pools that frames changed, also when the block's search
// was deferred behind a queued frame: quoting the whole table on the main loop
// held up the next block's frames.
func TestCanonicalReconcileSearchesAllPoolsWithoutTheFastLane(t *testing.T) {
	r, event := reactiveFixture(t)
	route := event.Result.Opportunities[0].Cycle
	r.searchOptions = arb.RankedOptions{SwapFee: dex.SwapFee, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == route.Start
	}}
	r.reactive.settings.document.FastLane = true
	sink := &captureSearch{}
	r.worker, r.search = &arb.Worker{}, pipeline.NewSearchScheduler(sink, 1, nil)
	version := domain.StateVersion{Source: domain.SourceFast, Anchor: r.store.Block(), PrefixHash: domain.Hash{7}}
	r.tentative = version
	view := r.store.View()
	quoted := func() bool {
		r.reactive.offerMu.Lock()
		defer r.reactive.offerMu.Unlock()
		offer := r.reactive.fastSlot
		r.reactive.fastSlot = nil
		return offer != nil
	}
	if err := r.submitSearch(t.Context(), view, route.Pools, version); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 || quoted() {
		t.Fatal("canonical reconcile skipped the search or quoted the fast lane", len(sink.jobs))
	}
	// The block's search waits for a queued frame that changed an unrelated pool.
	r.inputRevision.Store(1)
	if err := r.submitSearch(t.Context(), view, route.Pools, version); err != nil {
		t.Fatal(err)
	}
	r.appliedRevision = 1
	unrelated := domain.PoolKey{Kind: domain.PoolKindV2, Address: addressAt(30)}
	if err := r.submit(t.Context(), view, []domain.PoolKey{unrelated}, version); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 2 || len(sink.jobs[1].Dirty) != len(route.Pools)+1 {
		t.Fatal("frame search lost the deferred block's pools", len(sink.jobs))
	}
	if quoted() {
		t.Fatal("deferred block's pools reached the fast lane")
	}
	if err := r.submit(t.Context(), view, route.Pools[:1], version); err != nil {
		t.Fatal(err)
	}
	if !quoted() {
		t.Fatal("frame on the route's pool did not reach the fast lane", r.reactive.takeSkips())
	}
}
