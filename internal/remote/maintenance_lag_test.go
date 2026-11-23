package remote

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestNearTipCanonicalLagDoesNotStarveDiscovery(t *testing.T) {
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
	d := &poolDiscovery{jobs: make(chan discoveryTask, 1), ctx: ctx, config: discovery.Config{RefreshBlocks: 120}}
	r := &runtime{store: store, fast: branch, worker: worker, topology: topology, discovery: d, search: pipeline.NewSearchScheduler(sink, 1, nil)}
	next := store.Block()
	next.Number++
	next.ParentHash = store.Block().Hash
	next.Hash = domain.Hash{11}
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}, catchingUp: true}); err != nil {
		t.Fatal(err)
	}
	if len(d.jobs) != 0 {
		t.Fatal("historical catch-up started discovery")
	}
	next.Number++
	next.ParentHash, next.Hash = next.Hash, domain.Hash{12}
	// The producer has validated this block and observed only its immediate
	// successor. That routine RPC lag must not freeze maintenance indefinitely.
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}, catchingUp: true, nearTip: true}); err != nil {
		t.Fatal(err)
	}
	if len(d.jobs) != 1 {
		t.Fatal("one-block RPC lag starved discovery")
	}
	job := <-d.jobs
	defer job.reader.Close()
	defer d.cancelTask()
	if job.reader.block.Ref != next || len(sink.jobs) != 0 {
		t.Fatal("maintenance used stale state or enabled historical search")
	}
}
