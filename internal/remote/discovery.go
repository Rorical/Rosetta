package remote

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/catalog"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type discoveryTask struct {
	ctx             context.Context
	topology        *pipeline.Topology
	reader          *snapshot
	view            poolstate.View
	activity        map[domain.PoolKey]discovery.Activity
	activityRecords []discovery.ActivityRecord
	graph           uint64
	generation      uint64
	urgent          bool
}
type discoveryAnswer struct {
	anchor            domain.BlockRef
	added             *poolstate.Store
	topology          *pipeline.Topology
	graph, generation uint64
	err               error
}
type poolDiscovery struct {
	config  discovery.Config
	logs    discovery.LogSource
	history discovery.LogSource // full-range exact-ID V4 Initialize lookups
	// state, when set, serves the round's reads at its anchor: an archive
	// endpoint for a primary that keeps little history (discoveryReader).
	state *rpc.Client
	// tailBlocks bounds the canonical blocks retained to replay a round's
	// additions up to the head (appendTail).
	tailBlocks      int
	index           *discovery.CreationIndex
	activity        discovery.ActivityBook
	jobs            chan discoveryTask
	results         chan discoveryAnswer
	cancel          context.CancelFunc
	cancelTask      context.CancelFunc
	ctx             context.Context
	scanGeneration  uint64
	probe           discovery.ActiveProbe
	verdicts        map[domain.PoolKey]discovery.Verdict // worker-owned, see discovery_verdicts.go
	chainID         uint64
	candidates      discovery.CandidateQueue
	markets         discovery.MarketSelector
	done            chan struct{}
	busy            bool
	last, minAnchor uint64
	generation      uint64
	urgent          bool
	tail            []canonicalUpdate
	bytes           int
	// deniedTokens caches deny_token_code_selectors verdicts; code at an
	// address does not change short of self-destruct. Worker-owned.
	deniedTokens map[domain.Address]bool
}

func newPoolDiscovery(ctx context.Context, r *runtime) (*poolDiscovery, error) {
	if r.cfg.DiscoveryConfig == "" {
		return nil, nil
	}
	if r.worker == nil {
		return nil, fmt.Errorf("discovery requires search")
	}
	raw, err := os.ReadFile(r.cfg.DiscoveryConfig)
	if err != nil {
		return nil, err
	}
	cfg, err := discovery.LoadConfigForNetwork(raw, &r.cfg.Network.DEX)
	if err != nil {
		return nil, err
	}
	d := &poolDiscovery{config: cfg, chainID: r.cfg.ChainID(), logs: &discovery.WindowLogs{Source: r.client, Window: cfg.LookbackBlocks}, jobs: make(chan discoveryTask, 1), results: make(chan discoveryAnswer, 1), done: make(chan struct{})}
	// The primary RPC may cap eth_getLogs ranges; an archive provider without
	// that cap finds a months-old V4 Initialize by pool ID in one request.
	if endpoint := os.Getenv("ROSETTA_HISTORY_LOGS_RPC_URL"); endpoint != "" {
		d.history = discovery.HistoryLogs{Source: rpc.NewClient(endpoint, rpc.Options{Timeout: 20 * time.Second})}
	}
	// Robinhood's Nitro follower serves state for only the last 128 blocks,
	// 13 seconds; a round takes minutes, so its anchor reads failed there.
	if endpoint := os.Getenv("ROSETTA_DISCOVERY_STATE_RPC_URL"); endpoint != "" {
		d.state = rpc.NewClient(endpoint, rpc.Options{Timeout: 20 * time.Second, MaxRetries: 2, RetryReads: true, Backoff: 200 * time.Millisecond})
	}
	d.tailBlocks = max(discovery.ReplayBlockLimit, int(r.cfg.Network.Blocks(discoveryTailSpan)))
	if cfg.InventoryPath != "" {
		identity := discovery.IndexIdentity(r.cfg.ChainID(), cfg)
		d.index = &discovery.CreationIndex{Source: r.client, Path: cfg.InventoryPath, ChainID: r.cfg.ChainID(), Identity: identity}
		anchor, err := d.index.Load()
		if err != nil {
			return nil, err
		}
		if anchor.Hash != (domain.Hash{}) {
			current, err := r.client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", anchor.Number))
			if err != nil {
				return nil, err
			}
			if current.Ref.Hash != anchor.Hash {
				d.index.Reset()
			} else {
				d.minAnchor = anchor.Number
				if err := d.activity.Restore(d.index.Activity(), anchor); err != nil {
					return nil, err
				}
				d.restoreVerdicts()
			}
		}
		d.logs = d.index
	}
	workerCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.ctx = workerCtx
	go func() {
		defer close(d.done)
		for {
			select {
			case <-workerCtx.Done():
				return
			case job := <-d.jobs:
				answer := d.build(workerCtx, r, job)
				job.reader.Close()
				select {
				case d.results <- answer:
				case <-workerCtx.Done():
					return
				}
			}
		}
	}()
	return d, nil
}
func (d *poolDiscovery) close() {
	if d == nil {
		return
	}
	d.cancel()
	<-d.done
}
func (d *poolDiscovery) schedule(r *runtime) {
	if d == nil || d.busy || r.catchingUp && !r.nearTip || r.block.Ref.Number < d.minAnchor {
		return
	}
	if d.last != 0 && r.block.Ref.Number >= d.last {
		since := r.block.Ref.Number - d.last
		due := d.config.RefreshBlocks == 0 || since >= d.config.RefreshBlocks
		urgent := d.urgent && since >= min(r.cfg.Network.Blocks(discoveryUrgentGap), max(d.config.RefreshBlocks, 1))
		if !due && !urgent {
			return
		}
	}
	jobCtx, cancel := context.WithCancel(d.ctx)
	d.cancelTask = cancel
	d.jobs <- discoveryTask{ctx: jobCtx, topology: r.topology, reader: d.reader(r), view: r.store.View(), activity: d.activity.Snapshot(), activityRecords: d.activity.Records(), graph: r.search.GraphVersion, generation: d.generation}
	d.busy = true
	d.urgent = false
	d.last = r.block.Ref.Number
}
func (d *poolDiscovery) observe(update canonicalUpdate, r *runtime) {
	if d == nil {
		return
	}
	if update.reset != nil {
		d.generation++
		d.minAnchor = 0
		d.last = 0
		d.tail = nil
		d.bytes = 0
		d.activity = discovery.ActivityBook{}
		if d.cancelTask != nil {
			d.cancelTask()
		}
		return
	}
	logs := ingest.ReceiptLogs(update.block.Ref, update.transactions)
	var keys []domain.PoolKey
	for _, p := range r.store.Pools() {
		keys = append(keys, p.Key)
	}
	window := d.config.LookbackBlocks
	if d.config.Selection != nil {
		window = d.config.Selection.ActivityWindow
	}
	d.activity.Observe(update.block.Ref, logs, keys, window)
	for _, log := range logs {
		for _, emitter := range d.config.Emitters() {
			if emitter.Matches(log.Address, log.Topics) {
				d.urgent = true
			}
		}
	}
	d.appendTail(update)
}
func (d *poolDiscovery) consume(ctx context.Context, r *runtime, answer discoveryAnswer) error {
	d.busy = false
	if d.cancelTask != nil {
		d.cancelTask()
		d.cancelTask = nil
	}
	stale := func() error {
		r.metrics.Add(telemetry.Standard, telemetry.DiscoveryStale, 1)
		return nil
	}
	if answer.generation != d.generation || answer.graph != r.search.GraphVersion {
		return stale()
	}
	if answer.err != nil {
		slog.Warn("pool discovery failed", "error", answer.err)
		r.metrics.Add(telemetry.Standard, telemetry.DiscoveryErrors, 1)
		return nil
	}
	if answer.topology == nil {
		return nil
	}
	added := answer.added
	if added.Block() != r.store.Block() {
		branch, err := ingest.NewTransactionBranch(added)
		if err != nil {
			return err
		}
		for _, update := range d.tail {
			if update.block.Ref.Number <= added.Block().Number {
				continue
			}
			if update.block.Ref.ParentHash != branch.Base().Block().Hash {
				slog.Warn("pool discovery replay tail incomplete", "anchor", answer.anchor.Number, "head", r.store.Block().Number, "retained_blocks", len(d.tail))
				return stale()
			}
			if err := branch.Begin(update.block.Ref); err != nil {
				return stale()
			}
			for _, tx := range update.transactions {
				if _, err := branch.Apply(ctx, tx); err != nil {
					return stale()
				}
			}
			if err := branch.Commit(update.block.Ref, update.block.Transactions); err != nil {
				return stale()
			}
		}
		added = branch.Base()
		if added.Block() != r.store.Block() {
			slog.Warn("pool discovery replay tail did not reach head", "anchor", answer.anchor.Number, "head", r.store.Block().Number, "retained_blocks", len(d.tail))
			return stale()
		}
	}
	merged := r.store.View().Fork()
	var adopted uint64
	for _, p := range added.Pools() {
		if _, exists := merged.Pool(p.Key); exists {
			continue
		}
		adopted++
		if pipeline.DynamicFee(p.Key.Kind) && p.Slipstream == nil {
			if r.waitingFees == nil {
				r.waitingFees = make(map[domain.PoolKey]bool)
			}
			r.waitingFees[p.Key] = true
		}
		if err := merged.Upsert(p); err != nil {
			return err
		}
		if deep, ok := added.View().Deep(p.Key); ok {
			if err := merged.SetDeep(p.Key, deep); err != nil {
				return err
			}
		}
	}
	merged.Commit()
	merged.ClearDirty()
	graph := r.search.GraphVersion + 1
	if err := r.worker.ReplaceEngine(answer.topology.Engine, graph); err != nil {
		return err
	}
	r.topology = answer.topology
	r.search.GraphVersion = graph
	r.metrics.Add(telemetry.Standard, telemetry.DiscoveryCompleted, 1)
	r.metrics.Add(telemetry.Standard, telemetry.DiscoveryPoolsAdded, adopted)
	pipeline.ObserveTopology(r.metrics, graph, r.topology.Table.Len(), r.topology.Truncated)
	r.search.StandardPending = domain.StateVersion{}
	r.search.TentativePending = domain.StateVersion{}
	r.store = merged
	if err := r.capture.Snapshot(merged.View()); err != nil {
		return err
	}
	r.advance()
	keys := []domain.PoolKey{}
	for _, p := range merged.Pools() {
		keys = append(keys, p.Key)
	}
	if r.flashLogs != nil || r.cfg.FastURL != "" {
		if err := r.refreshTentativeDependencies(ctx, keys); err != nil {
			return err
		}
	} else if err := r.resetTentative(); err != nil {
		return err
	}
	slog.Info("remote pool graph installed", "pools", len(keys), "cycles", r.topology.Table.Len(), "truncated", r.topology.Truncated, "graph_version", graph)
	return r.submit(ctx, merged.View(), keys, r.version)
}
func (d *poolDiscovery) flush(ctx context.Context, r *runtime) error {
	if d == nil {
		return nil
	}
	for d.busy {
		select {
		case answer := <-d.results:
			if err := d.consume(ctx, r, answer); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// candidateCreations merges indexed old-market metadata with incremental logs.
func (d *poolDiscovery) candidateCreations(ctx context.Context, job discoveryTask) ([]discovery.Creation, error) {
	loader, err := discovery.New(d.logs, job.reader, d.config)
	if err != nil {
		return nil, err
	}
	from := uint64(0)
	if job.reader.block.Ref.Number > d.config.LookbackBlocks {
		from = job.reader.block.Ref.Number - d.config.LookbackBlocks
	}
	found, err := loader.ScanCreations(ctx, from, job.reader.block.Ref.Number)
	if err != nil {
		return nil, err
	}
	if d.index != nil {
		found = append(d.index.Records(), found...)
	}
	for _, seed := range d.config.Seeds {
		found = append(found, discovery.Creation{Spec: seed})
	}
	if d.index != nil {
		d.index.Remember(found)
	}
	seen := map[domain.PoolKey]bool{}
	var candidates []discovery.Creation
	for _, row := range found {
		key := row.Spec.PoolKey()
		if !d.config.AcceptSpec(row.Spec) {
			continue
		}
		if d.config.Selection != nil && row.Source != 0 && discovery.ActivityFor(job.activity, key).Swaps < d.config.Selection.MinSwaps {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, known := job.view.Pool(key); !known && !d.cooling(key, job.reader.block.Ref.Number) {
			candidates = append(candidates, row)
		}
	}
	limit := d.config.MaxPools
	if d.config.Selection != nil {
		limit = d.config.Selection.ProbeLimit
	}
	extra, err := d.probeUnknown(ctx, job, seen, discovery.ProbeAllowance(limit, len(candidates)), found)
	if err != nil {
		return nil, err
	}
	for _, row := range extra {
		if d.config.AcceptSpec(row.Spec) {
			candidates = append(candidates, row)
		}
	}
	if d.index != nil {
		d.index.Remember(extra)
	}
	return d.candidates.Next(candidates, job.activity, limit), nil
}

func discoverySpec(spec discovery.Spec) bootstrap.Spec {
	return bootstrap.Spec{Address: spec.Address, Kind: spec.Kind, PoolID: spec.PoolID, Hooks: spec.Hooks, Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee, TickSpacing: spec.TickSpacing}
}
func (d *poolDiscovery) verify(ctx context.Context, reader *snapshot, row discovery.Creation) (bootstrap.Spec, bool, error) {
	spec := discoverySpec(row.Spec)
	if spec.Kind == domain.PoolKindV4 {
		return catalog.VerifyV4Pool(ctx, reader, spec, reader.deployments.V4StateView)
	}
	if row.Source == 0 {
		for _, seed := range d.config.Seeds {
			if seed.PoolKey() == row.Spec.PoolKey() {
				return spec, true, nil
			}
		}
		return spec, false, fmt.Errorf("unconfigured discovery seed")
	}
	return catalog.VerifyPool(ctx, reader, catalog.Factory{Address: row.Emitter, Kind: spec.Kind}, spec)
}
