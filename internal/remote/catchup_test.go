package remote

import (
	"context"
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type captureSearch struct{ jobs []arb.SearchJob }

func (s *captureSearch) Submit(job arb.SearchJob) error { s.jobs = append(s.jobs, job); return nil }
func (*captureSearch) Current(arb.SearchEvent) bool     { return false }
func (*captureSearch) Accept(arb.SearchEvent) bool      { return false }

func TestCanonicalCatchupDefersSearchWithoutLosingPoolChanges(t *testing.T) {
	store, pools := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := arb.NewWorker(ctx, topology.Engine, arb.RankedOptions{Bounds: func(domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000)}, true
	}, SwapFee: dex.SwapFee})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); <-worker.Done() }()
	sink := &captureSearch{}
	r := &runtime{store: store, fast: branch, worker: worker, topology: topology, search: pipeline.NewSearchScheduler(sink, 1, nil)}
	next := store.Block()
	next.Number++
	next.ParentHash = store.Block().Hash
	next.Hash = domain.Hash{11}
	log := syncEvent(pools[0].Key, 3_000_000)
	tx := ingest.Transaction{Hash: domain.Hash{33}, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: next, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}, catchingUp: true}); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 0 {
		t.Fatal("historical search competes with canonical catchup")
	}
	current, _ := r.store.Pool(pools[0].Key)
	if current.Reserve1.Int64() != 3_000_000 {
		t.Fatal("catchup skipped pool state changes")
	}
	// The caught-up block has no pool logs. Its search must include earlier changes.
	tip := next
	tip.Number++
	tip.ParentHash = next.Hash
	tip.Hash = domain.Hash{12}
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: tip}}); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 {
		t.Fatal("caught-up search missing")
	}
	job := sink.jobs[0]
	defer job.Release()
	// Only what changed while catching up is searched again: rescreening every
	// pool after each one-block lag would rank the whole table several times
	// a minute.
	if job.Version.Anchor != tip || !slices.Equal(job.Dirty, []domain.PoolKey{pools[0].Key}) {
		t.Fatal("catchup changes or current position lost", job.Version, job.Dirty)
	}
}

// A process that has not searched its pools yet rescreens all of them once,
// at the first block it reaches the tip.
func TestFirstTipBlockRescreensEveryPool(t *testing.T) {
	store, pools := poolFixture(t)
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := arb.NewWorker(ctx, topology.Engine, arb.RankedOptions{Bounds: func(domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000)}, true
	}, SwapFee: dex.SwapFee})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); <-worker.Done() }()
	sink := &captureSearch{}
	r := &runtime{store: store, worker: worker, topology: topology, search: pipeline.NewSearchScheduler(sink, 1, nil), rescreen: true}
	next := store.Block()
	next.Number++
	next.ParentHash = store.Block().Hash
	next.Hash = domain.Hash{21}
	for i := range 2 {
		if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}}); err != nil {
			t.Fatal(err)
		}
		next.ParentHash, next.Number, next.Hash = next.Hash, next.Number+1, domain.Hash{byte(22 + i)}
	}
	if len(sink.jobs) != 2 || len(sink.jobs[0].Dirty) != len(pools) || len(sink.jobs[1].Dirty) != 0 {
		t.Fatal("first tip block did not rescreen every pool once", len(sink.jobs))
	}
	for _, job := range sink.jobs {
		job.Release()
	}
}
