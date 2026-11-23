package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func TestCanonicalPoolMetricsIncludeCatchupAndDeduplicateDirtyPools(t *testing.T) {
	store, pools := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	r := &runtime{store: store, fast: branch}
	next := store.Block()
	next.Number++
	next.ParentHash = next.Hash
	next.Hash = domain.Hash{11}
	log := syncEvent(pools[0].Key, 3_000_000)
	transactions := []ingest.Transaction{
		{Hash: domain.Hash{1}, Index: 0, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}},
		{Hash: domain.Hash{2}, Index: 1, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}},
	}
	update := canonicalUpdate{block: rpc.ExecutionBlock{Ref: next, Transactions: []domain.Hash{{1}, {2}}}, transactions: transactions, catchingUp: true}
	if err := r.acceptCanonical(ctx, update); err != nil {
		t.Fatal(err)
	}
	counters := registry.Snapshot().Sources["standard"].Counters
	for name, want := range map[string]uint64{"messages": 1, "receipts": 2, "logs": 2, "dirty_pools": 1} {
		if counters[name] != want {
			t.Errorf("%s = %d, want %d", name, counters[name], want)
		}
	}
	if registry.Snapshot().Sources["fast"].Counters["messages"] != 0 {
		t.Fatal("canonical metrics leaked to fast stream")
	}
	gauges := registry.Snapshot().Sources["standard"].Gauges
	if gauges["head_block"] != int64(next.Number) || gauges["head_timestamp_seconds"] != int64(next.Timestamp) || gauges["pools"] != int64(len(pools)) {
		t.Fatalf("canonical gauges: %+v", gauges)
	}
}

func TestFlashblockMetricsCountHeartbeatWithoutCountingDuplicateOrGap(t *testing.T) {
	store, pools := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	r := &runtime{store: store, fast: branch}
	block := feed.Flashblock{PayloadID: "0x0102030405060708", Number: 11, Base: &feed.FlashblockBase{Number: 11, Parent: store.Block().Hash}, Transactions: []feed.FlashTransaction{
		{Hash: domain.Hash{1}, Raw: []byte{1}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}},
		{Hash: domain.Hash{2}, Raw: []byte{2}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 4_000_000)}},
		{Hash: domain.Hash{3}, Raw: []byte{3}},
	}}
	for range 2 {
		if err := r.acceptFlashblock(ctx, block, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := feed.Flashblock{PayloadID: block.PayloadID, Number: 11, Index: 1}
	if err := r.acceptFlashblock(ctx, heartbeat, time.Time{}); err != nil {
		t.Fatal(err)
	}
	gap := heartbeat
	gap.Index = 3
	if err := r.acceptFlashblock(ctx, gap, time.Time{}); err != nil {
		t.Fatal(err)
	}
	counters := registry.Snapshot().Sources["fast"].Counters
	for name, want := range map[string]uint64{"messages": 2, "receipts": 3, "logs": 2, "dirty_pools": 1} {
		if counters[name] != want {
			t.Errorf("%s = %d, want %d", name, counters[name], want)
		}
	}
	if registry.Snapshot().Sources["standard"].Counters["messages"] != 0 {
		t.Fatal("fast metrics leaked to canonical stream")
	}
}

func TestFlashblockMetricsCountWaitingFrameOnlyAfterParentAccepted(t *testing.T) {
	store, _ := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	r := &runtime{store: store, fast: branch}
	block := feed.Flashblock{PayloadID: "0x0102030405060708", Number: 12, Base: &feed.FlashblockBase{Number: 12, Parent: domain.Hash{11}}}
	if err := r.acceptFlashblock(ctx, block, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if registry.Snapshot().Sources["fast"].Counters["messages"] != 0 {
		t.Fatal("waiting frame counted before parent verification")
	}
	next := store.Block()
	next.Number++
	next.ParentHash = next.Hash
	next.Hash = domain.Hash{11}
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}}); err != nil {
		t.Fatal(err)
	}
	if err := r.acceptFlashblock(ctx, block, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if registry.Snapshot().Sources["fast"].Counters["messages"] != 1 {
		t.Fatal("waiting frame missing or counted twice after adoption")
	}
}

func TestFlashblockLatencyRetainsOriginalArrivalWhileWaitingForParent(t *testing.T) {
	r, pools := flashChainRuntime(t)
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	arrived := time.Now().Add(-time.Second)
	if err := r.acceptFlashblock(ctx, chainFrame(pools[0], 12, 3_000_000), arrived); err != nil {
		t.Fatal(err)
	}
	if registry.Snapshot().Sources["fast"].Stages["feed_queue"].Count != 0 {
		t.Fatal("unapplied frame was measured as processed")
	}
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	if err := r.acceptCanonical(ctx, canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent}}); err != nil {
		t.Fatal(err)
	}
	stages := registry.Snapshot().Sources["fast"].Stages
	if stages["feed_queue"].Count != 1 || stages["feed_queue"].SumNS < uint64(time.Second) || stages["receipt_latency"].SumNS < uint64(time.Second) || stages["pool_apply"].Count != 1 {
		t.Fatal("parent wait was hidden from local latency", stages)
	}
	if r.fastReceivedAt != arrived {
		t.Fatal("candidate search trigger moved from arrival to submission")
	}
}

func TestArbitrumRPCFastLatencyUsesArrivalAndExcludesDuplicate(t *testing.T) {
	r, pools := flashChainRuntime(t)
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	arrived := time.Now().Add(-time.Second)
	message := feed.FastMessage{BlockNumber: 11, TxHash: domain.Hash{11}, RawTx: []byte{11}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}}}
	for revision := uint64(1); revision <= 2; revision++ {
		r.inputRevision.Store(revision)
		if err := r.acceptFastMessage(ctx, message, arrived, revision); err != nil {
			t.Fatal(err)
		}
	}
	source := registry.Snapshot().Sources["fast"]
	if source.Counters["receipts"] != 1 || source.Stages["feed_queue"].Count != 1 || source.Stages["feed_queue"].SumNS < uint64(time.Second) || source.Stages["receipt_latency"].SumNS < uint64(time.Second) {
		t.Fatal("Fast receipt timing lost arrival time or counted a duplicate", source)
	}
	if !r.Current(r.tentative) || r.fastReceivedAt != arrived {
		t.Fatal("duplicate changed the trigger time or failed to acknowledge the input revision")
	}
}

func TestPausedGateHoldsPlanWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	gate, err := control.OpenGate(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	var output bytes.Buffer
	r := &runtime{gate: gate, plans: json.NewEncoder(&output)}
	plan := &execution.ReadyPlan{}
	if err := r.deliver(ctx, telemetry.Standard, plan, time.Time{}); err != nil || output.Len() == 0 {
		t.Fatalf("open gate blocked delivery: %v", err)
	}
	output.Reset()
	if _, err := gate.Set(true, "operator", "incident", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, source := range []telemetry.Source{telemetry.Standard, telemetry.Fast} {
		if err := r.deliver(ctx, source, plan, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := registry.Snapshot()
	if output.Len() != 0 || snapshot.Sources["standard"].Counters["plans_held"] != 1 || snapshot.Sources["fast"].Counters["plans_held"] != 1 {
		t.Fatalf("paused delivery wrote %d bytes: %+v", output.Len(), snapshot.Sources["standard"].Counters["plans_held"])
	}
}

func TestDeliveredPlanCountsReadyAndLatency(t *testing.T) {
	registry := &telemetry.Registry{}
	ctx := telemetry.WithRegistry(t.Context(), registry)
	r := &runtime{plans: json.NewEncoder(&bytes.Buffer{})}
	if err := r.deliver(ctx, telemetry.Fast, &execution.ReadyPlan{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	fast := registry.Snapshot().Sources["fast"]
	if fast.Counters["ready_plans"] != 1 || fast.Stages["ready_latency"].Count != 1 {
		t.Fatalf("ready_plans = %d, ready_latency = %d", fast.Counters["ready_plans"], fast.Stages["ready_latency"].Count)
	}
}

func TestTentativeResetCountsStreamInvalidation(t *testing.T) {
	registry := &telemetry.Registry{}
	store, _ := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{store: store, fast: branch, metrics: registry}
	if err := r.resetTentative(); err != nil {
		t.Fatal(err)
	}
	if got := registry.Snapshot().Sources["fast"].Counters["stream_invalidations"]; got != 1 {
		t.Fatalf("stream_invalidations = %d", got)
	}
}

func TestDiscoveryOutcomesAreCounted(t *testing.T) {
	registry := &telemetry.Registry{}
	r := &runtime{metrics: registry, search: &pipeline.SearchScheduler{GraphVersion: 1}}
	d := &poolDiscovery{generation: 2}
	for _, answer := range []discoveryAnswer{{generation: 1, graph: 1}, {generation: 2, graph: 9}, {generation: 2, graph: 1, err: errors.New("scan failed")}} {
		if err := d.consume(t.Context(), r, answer); err != nil {
			t.Fatal(err)
		}
	}
	counters := registry.Snapshot().Sources["standard"].Counters
	if counters["discovery_stale"] != 2 || counters["discovery_errors"] != 1 || counters["discovery_completed"] != 0 {
		t.Fatalf("discovery counters: stale=%d errors=%d completed=%d", counters["discovery_stale"], counters["discovery_errors"], counters["discovery_completed"])
	}
}
