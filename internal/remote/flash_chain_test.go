package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func flashChainRuntime(t *testing.T) (*runtime, []pool.State) {
	t.Helper()
	store, pools := poolFixture(t)
	fast, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &runtime{store: store, fast: fast}, pools
}

func chainFrame(p pool.State, number uint64, reserve int64) feed.Flashblock {
	return feed.Flashblock{PayloadID: domain.Hash{byte(number)}.Hex(), Number: number, Base: &feed.FlashblockBase{
		Number: number, Parent: domain.Hash{byte(number - 1)},
	}, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{byte(number)}, Raw: []byte{byte(number)}, Successful: true, Logs: []feed.FastLog{syncEvent(p.Key, reserve)}}}}
}

func TestFlashblocksAdvanceAcrossBlocksBeforeRPC(t *testing.T) {
	r, pools := flashChainRuntime(t)
	for number := uint64(11); number <= 13; number++ {
		if err := r.acceptFlashblock(t.Context(), chainFrame(pools[0], number, int64(number)*300000), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if r.tentative.BlockNumber != number {
			t.Fatalf("feed stopped at RPC parent: version=%+v, frame=%d", r.tentative, number)
		}
	}
	if r.store.Block().Number != 10 {
		t.Fatal("feed advanced the canonical store")
	}
	original, _ := r.store.Pool(pools[0].Key)
	if original.Reserve1.Int64() != 2_000_000 {
		t.Fatal("feed modified canonical pool reserves")
	}
}

func TestFlashblocksCatalogRefreshReplaysPreviouslyUntrackedLogs(t *testing.T) {
	r, pools := flashChainRuntime(t)
	newPool := pools[0]
	newPool.Key.Address = domain.Address{99}
	for number := uint64(11); number <= 13; number++ {
		if err := r.acceptFlashblock(t.Context(), chainFrame(newPool, number, int64(number)*300000), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	old := r.tentative
	if err := r.store.Upsert(newPool); err != nil {
		t.Fatal(err)
	}
	if err := r.refreshTentativeDependencies(t.Context(), []domain.PoolKey{newPool.Key}); err != nil {
		t.Fatal(err)
	}
	view, version, ok := r.flashLogs.View()
	updated, found := view.Pool(newPool.Key)
	canonical, _ := r.store.Pool(newPool.Key)
	if !ok || !found || version.BlockNumber != 13 || updated.Reserve1.Int64() != 3_900_000 {
		t.Fatal("catalog refresh lost future logs for the new pool")
	}
	if canonical.Reserve1.Cmp(newPool.Reserve1) != 0 || r.Current(old) {
		t.Fatal("catalog refresh modified canonical reserves or retained old quotes")
	}
	next := chainFrame(newPool, 13, 4_000_000)
	next.Index, next.Base = 1, nil
	next.Transactions[0].Hash = domain.Hash{100}
	if err := r.acceptFlashblock(t.Context(), next, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.flash.index != 1 || r.tentative.BlockNumber != 13 {
		t.Fatal("catalog refresh discarded the stream cursor")
	}
}

func TestLateCanonicalBlockPreservesNewerFlashblocksCursor(t *testing.T) {
	r, pools := flashChainRuntime(t)
	first := chainFrame(pools[0], 11, 3_000_000)
	for _, frame := range []feed.Flashblock{first, chainFrame(pools[0], 12, 3_200_000), chainFrame(pools[0], 13, 3_400_000)} {
		if err := r.acceptFlashblock(t.Context(), frame, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	previous := r.tentative
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	log := first.Transactions[0].Logs[0]
	tx := ingest.Transaction{Hash: first.Transactions[0].Hash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	if r.tentative.BlockNumber != 13 || r.tentative.Anchor != parent || r.Current(previous) {
		t.Fatalf("canonical catch-up discarded newer feed: %+v", r.tentative)
	}
	suffix := feed.Flashblock{PayloadID: chainFrame(pools[0], 13, 0).PayloadID, Number: 13, Index: 1, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{99}, Raw: []byte{99}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_500_000)}}}}
	if err := r.acceptFlashblock(t.Context(), suffix, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if r.tentative.BlockNumber != 13 || r.tentative.TxIndex != 1 {
		t.Fatal("could not extend the preserved payload")
	}
}

func TestCanonicalRebaseResubmitsAllAffectedFuturePools(t *testing.T) {
	r, pools := flashChainRuntime(t)
	sink := &captureSearch{}
	r.worker = &arb.Worker{}
	r.search = pipeline.NewSearchScheduler(sink, 1, nil)
	r.cfg.RPCSearchRefresh = "logs"
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	r.topology = topology
	first := chainFrame(pools[0], 11, 3_000_000)
	for _, frame := range []feed.Flashblock{first, chainFrame(pools[1], 12, 3_200_000)} {
		if err := r.acceptFlashblock(t.Context(), frame, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	parent := r.store.Block()
	parent.Number, parent.ParentHash, parent.Hash = 11, parent.Hash, domain.Hash{11}
	log := first.Transactions[0].Logs[0]
	tx := ingest.Transaction{Hash: first.Transactions[0].Hash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	sink.jobs = nil
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	for _, job := range sink.jobs {
		if job.Version.Source != domain.SourceFast {
			continue
		}
		if len(job.Dirty) != 2 || job.Dirty[0] != pools[0].Key || job.Dirty[1] != pools[1].Key {
			t.Fatal("canonical rebase dropped future dirty paths", job.Dirty)
		}
		return
	}
	t.Fatal("rebased Fast search missing")
}

func TestFlashblocksQueuedStartupAttachesMultiplePayloads(t *testing.T) {
	r, pools := flashChainRuntime(t)
	arrived := time.Now().Add(-time.Second)
	for number := uint64(12); number <= 14; number++ {
		if err := r.acceptFlashblock(t.Context(), chainFrame(pools[0], number, int64(number)*300000), arrived); err != nil {
			t.Fatal(err)
		}
	}
	if r.tentative != (domain.StateVersion{}) || len(r.flashWaiting) != 3 {
		t.Fatal("unattached stream became quotable or lost earlier payloads")
	}
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent}}); err != nil {
		t.Fatal(err)
	}
	view, version, ok := r.flashLogs.View()
	p, _ := view.Pool(pools[0].Key)
	if !ok || version.BlockNumber != 14 || version.Anchor != parent || p.Reserve1.Int64() != 4_200_000 || len(r.flashWaiting) != 0 {
		t.Fatal("RPC attachment dropped contiguous future blocks")
	}
}

func TestFlashblocksKnownNewInputRetiresQuotesBeforeWriterRuns(t *testing.T) {
	r, pools := flashChainRuntime(t)
	r.inputRevision.Store(1)
	r.processingRevision = 1
	if err := r.acceptFlashblock(t.Context(), chainFrame(pools[0], 11, 3_000_000), time.Now()); err != nil {
		t.Fatal(err)
	}
	previous := r.tentative
	if !r.Current(previous) {
		t.Fatal("current applied frame rejected")
	}
	r.inputRevision.Add(1)
	if r.Current(previous) {
		t.Fatal("queued newer frame left an old quote current")
	}
	suffix := feed.Flashblock{PayloadID: chainFrame(pools[0], 11, 0).PayloadID, Number: 11, Index: 1}
	r.processingRevision = 2
	if err := r.acceptFlashblock(t.Context(), suffix, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !r.Current(r.tentative) {
		t.Fatal("drained frame did not reopen the quote gate")
	}
	r.inputRevision.Add(1)
	if r.Current(r.tentative) {
		t.Fatal("disconnect waited for writer acknowledgement to retire quotes")
	}
}

func TestFlashblocksGapDiscardsFutureStateAndRecoversFromCanonicalBase(t *testing.T) {
	r, pools := flashChainRuntime(t)
	first := chainFrame(pools[0], 11, 3_000_000)
	if err := r.acceptFlashblock(t.Context(), first, time.Time{}); err != nil {
		t.Fatal(err)
	}
	old := r.tentative
	gap := feed.Flashblock{PayloadID: first.PayloadID, Number: 11, Index: 2}
	if err := r.acceptFlashblock(t.Context(), gap, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if r.Current(old) {
		t.Fatal("gap retained a usable quote")
	}
	if _, _, ok := r.flashLogs.View(); ok {
		t.Fatal("gap retained pool effects")
	}
	if err := r.acceptFlashblock(t.Context(), chainFrame(pools[0], 12, 4_000_000), time.Time{}); err != nil {
		t.Fatal(err)
	}
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent}}); err != nil {
		t.Fatal(err)
	}
	if r.tentative.BlockNumber != 12 || r.tentative.Anchor != parent {
		t.Fatal("complete new payload could not recover after gap")
	}
}

// Reactive ignores standard results while the stream is attached, so the
// canonical rescreen must not occupy the search worker ahead of the next frame.
// Without an attached stream it remains the only search.
func TestCanonicalBlockSkipsStandardSearchWhileStreamAttached(t *testing.T) {
	r, pools := flashChainRuntime(t)
	sink := &captureSearch{}
	r.worker = &arb.Worker{}
	r.search = pipeline.NewSearchScheduler(sink, 1, nil)
	r.cfg.RPCSearchRefresh = "logs"
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	r.topology = topology
	standard := func() bool {
		for _, job := range sink.jobs {
			if job.Version.Source == domain.SourceStandard {
				return true
			}
		}
		return false
	}
	first := chainFrame(pools[0], 11, 3_000_000)
	for _, frame := range []feed.Flashblock{first, chainFrame(pools[1], 12, 3_200_000)} {
		if err := r.acceptFlashblock(t.Context(), frame, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	parent := r.store.Block()
	parent.Number, parent.ParentHash, parent.Hash = 11, parent.Hash, domain.Hash{11}
	log := first.Transactions[0].Logs[0]
	tx := ingest.Transaction{Hash: first.Transactions[0].Hash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	sink.jobs = nil
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	if !r.flash.active || standard() {
		t.Fatal("standard search ran while the stream was attached", r.flash.active)
	}
	if err := r.resetTentative(); err != nil {
		t.Fatal(err)
	}
	next := parent
	next.Number, next.ParentHash, next.Hash = 12, parent.Hash, domain.Hash{12}
	sink.jobs = nil
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}}); err != nil {
		t.Fatal(err)
	}
	if !standard() {
		t.Fatal("standard search skipped without an attached stream")
	}
}
