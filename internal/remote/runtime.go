package remote

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

type runtime struct {
	feedArrivals       *feedArrivals // standard feed arrivals by block hash; nil without a sequencer feed
	risks              *tokenrisk.Registry
	poolRisks          *poolrisk.Registry
	reactiveRevision   atomic.Uint64
	reactive           *reactiveRuntime
	executionPaper     *executionPaper
	ticks              *tickRepair
	waitingFees        map[domain.PoolKey]bool
	capture            *backtest.Capture
	cfg                config.Config
	client             *rpc.Client
	store              *poolstate.Store
	block              rpc.ExecutionBlock
	feeParams          opFeeParams // OP fee parameters of block
	version, tentative domain.StateVersion
	epoch              uint64
	catchingUp         bool
	// rescreen searches every pool at the next tip block: a new process has
	// searched none of them, and a reset replaced the store.
	rescreen bool
	// catchUp holds what to search once the follower reaches the tip: pools
	// changed by skipped blocks and pools whose search catch-up dropped.
	catchUp         map[domain.PoolKey]struct{}
	snapshotWritten bool
	nearTip         bool
	canonicalLag    uint64 // blocks the canonical follower trails the RPC tip
	canonicalReader *snapshot
	discovery       *poolDiscovery
	research        *pipeline.Research
	fast            *ingest.FastBranch
	flash           flashCursor
	flashPrefix     flashPrefix
	flashLogs       *ingest.LogBranch
	flashStart      domain.Hash
	fastReceivedAt  time.Time
	// lenders caches flashLenders for one pool set.
	lenders struct {
		pools []pool.State
		graph uint64
		count int
	}
	// taxPools indexes the pools whose Swaps can show a transfer tax.
	taxPools struct {
		pools map[domain.Address]pool.State
		graph uint64
		count int
	}
	deferred deferredFrames
	// fastQuoted is the routes the fast lane quoted for its latest version;
	// the full search's admitted routes are classified against it.
	fastQuoted struct {
		version  domain.StateVersion
		routes   map[string]struct{}
		selected map[string]struct{}
	}
	// blockFrames remembers, for the last few blocks, the pools fast frames
	// changed and the routes the fast lane quoted (fastCoverage).
	blockFrames map[uint64]*frameBlock
	// silence tracks when canonical blocks and reactive admissions last
	// arrived (checkSilence).
	silence silence
	// frameSteps times the main loop's work on the latest fast frame.
	frameSteps frameSteps
	// fastDequeuedAt is when the main loop took the frame behind fastReceivedAt.
	fastDequeuedAt     time.Time
	inputRevision      atomic.Uint64
	processingRevision uint64
	appliedRevision    uint64
	flashWaiting       []pendingFlashblock
	flashWaitingBytes  int
	policy             pipeline.SearchPolicy
	execution          planning.Policy
	topology           *pipeline.Topology
	worker             *arb.Worker
	searchOptions      arb.RankedOptions // bounds, fees and risk model shared by the worker and the fast lane
	survival           *routeSurvival    // route survival by length; paper outcomes refine the prior
	search             *pipeline.SearchScheduler
	confirmation       *pipeline.ConfirmationWorker
	flashConfirmation  *pipeline.ConfirmationWorker
	flashSnapshots     *flashSnapshotSource
	planPending        bool
	gate               *control.Gate
	metrics            *telemetry.Registry
	candidates, plans  *json.Encoder
	candidateFile      *os.File
	runs               *runRotation // nil without -run-root
}

type streamEvent struct {
	revision     uint64
	receivedAt   time.Time
	reset        bool
	flashPending bool
	flash        *feed.Flashblock
	fast         *feed.FastMessage
	accepted     chan struct{}
}

func run(ctx context.Context, cfg config.Config) (result error) {
	ctx, closeControl, err := control.Start(ctx, cfg, "rpc")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, closeControl()) }()
	registry := telemetry.FromContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = rpc.WithRequestPolicy(ctx, rpc.NewRequestPolicy(rpc.Options{MinInterval: cfg.RPCMinInterval, MaxCalls: cfg.RPCMaxCalls, Observe: registry.ObserveRPC, ObserveResponse: registry.ObserveRPCResponse}))
	client := rpc.NewClient(cfg.RPCURL, rpc.Options{Timeout: 10 * time.Second, MaxRetries: 2, RetryReads: true, Backoff: 100 * time.Millisecond})
	var chain string
	if err := client.Call(ctx, "eth_chainId", []any{}, &chain); err != nil {
		return err
	}
	id, err := quantity(chain)
	if err != nil || id != cfg.ChainID() {
		return fmt.Errorf("RPC chain identity differs from selected network")
	}
	if cfg.EventSource == "flashblocks" && !cfg.FlashblocksReceiptsRPC {
		if err := checkFlashblocksStream(rpc.WithCaller(ctx, rpc.CallerFlashReceipts), cfg.FlashblocksURL, client); err != nil {
			return err
		}
	}
	if (cfg.TokenRiskFile == "" || cfg.PoolRiskFile == "") && cfg.ExecutionPaperConfig != "" {
		paperPolicy, _, e := loadExecutionPaper(cfg.ExecutionPaperConfig)
		if e != nil {
			return e
		}
		if cfg.TokenRiskFile == "" {
			cfg.TokenRiskFile = paperPolicy.Output + ".token-risks.jsonl"
		}
		if cfg.PoolRiskFile == "" {
			cfg.PoolRiskFile = paperPolicy.Output + ".pool-risks.jsonl"
		}
	}
	r := &runtime{cfg: cfg, client: client, gate: control.FromContext(ctx), metrics: registry, rescreen: true}
	r.risks, err = tokenrisk.Open(cfg.TokenRiskFile, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()})
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.risks.Close()) }()
	r.poolRisks, err = poolrisk.Open(cfg.PoolRiskFile, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()})
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.poolRisks.Close()) }()
	radius := int16(1)
	if cfg.SearchConfig != "" {
		r.policy, err = pipeline.LoadSearchPolicy(cfg.SearchConfig)
		if err != nil {
			return err
		}
		radius = r.policy.WordRadius
		slog.Info("search route budget", "max_cycles", r.policy.MaxCycles, "max_depth", r.policy.Depth, "source", cmp.Or(r.policy.MaxCyclesSource, "search config"))
	}
	// Read before the bootstrap, so a broken policy costs no pool reads.
	wrapped, err := wrapNativeToken(cfg)
	if err != nil {
		return err
	}
	store, specs, block, err := bootstrapPools(rpc.WithCaller(ctx, rpc.CallerBootstrap), cfg, client, radius)
	if err != nil {
		return err
	}
	// The snapshot and the catalog know nothing of the reactive policy; the
	// wrap pool follows it on every start, before anything reads the store.
	if specs, err = setWrapPool(store, specs, wrapped); err != nil {
		return err
	}
	// A catalog bootstrap reads every pool, about 15,000 calls on Arbitrum One.
	// Checkpoint it at once, so a restart after a crash catches up from here
	// instead of reading every pool again.
	if cfg.PoolSnapshot == "" && cfg.PoolSnapshotOut != "" && cfg.RunRoot == "" {
		if err := writePoolSnapshot(cfg.PoolSnapshotOut, store); err != nil {
			return fmt.Errorf("bootstrap checkpoint: %w", err)
		}
	}
	// The run directory opens once the provider has answered the bootstrap,
	// so a restart loop against a failing provider leaves no empty runs.
	if cfg.RunRoot != "" {
		runs, err := openRunRoot(ctx, &cfg)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, runs.close(registry)) }()
		r.cfg, r.runs = cfg, runs
	}
	r.store, r.block = store, block
	r.waitingFees = make(map[domain.PoolKey]bool)
	for _, p := range store.Pools() {
		if pipeline.DynamicFee(p.Key.Kind) && p.Slipstream == nil {
			r.waitingFees[p.Key] = true
		}
	}
	r.capture, err = backtest.NewCapture(cfg.ReplayOut, cfg.ChainID())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.capture.Close()) }()
	if err := r.capture.Snapshot(store.View()); err != nil {
		return err
	}
	options := ingest.FastOptions{}
	if cfg.FastURL != "" {
		options.FirstTxIndex = uint64(cfg.FastFirstIndex)
		options.CanonicalFirstTxIndex = uint64(cfg.FastCanonicalFirstIndex)
	}
	r.fast, err = ingest.NewFastBranch(store, options)
	if err != nil {
		return err
	}
	var files []*os.File
	defer func() {
		for _, file := range files {
			result = errors.Join(result, file.Close())
		}
	}()
	open := func(path string) (*json.Encoder, error) {
		if path == "" {
			return json.NewEncoder(io.Discard), nil
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		return json.NewEncoder(file), nil
	}
	if cfg.CandidateOut != "" {
		if r.candidateFile, err = os.OpenFile(cfg.CandidateOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return err
		}
		// Rotation closes earlier candidate files; this closes the newest.
		defer func() { result = errors.Join(result, r.candidateFile.Close()) }()
		r.candidates = json.NewEncoder(r.candidateFile)
	}
	r.plans, err = open(cfg.PlanOut)
	if err != nil {
		return err
	}
	if cfg.SearchConfig != "" {
		r.topology, err = pipeline.BuildTopology(ctx, store.Pools(), r.topologyOptions())
		if err != nil {
			return err
		}
		r.searchOptions = arb.RankedOptions{Bounds: r.policy.Bounds, SwapFee: dex.SwapFee, Model: pipeline.RiskRanking(r.risks.Check, r.poolRisks.Check)}
		workerOptions := r.searchOptions
		workerOptions.StreamInitial, workerOptions.Diverse = true, true
		workerOptions.ExactK = r.policy.ExactK
		if r.topology.Fast != nil {
			// The fast lane ranks and sizes on the main loop when a frame
			// arrives; a full search holding every core made it wait for one.
			workerOptions.ReservedCPUs = fastLaneCPUs
		}
		r.worker, err = arb.NewWorker(ctx, r.topology.Engine, workerOptions)
		if err != nil {
			return err
		}
		r.search = pipeline.NewSearchScheduler(r.worker, 1, registry)
		pipeline.ObserveTopology(registry, 1, r.topology.Table.Len(), r.topology.Truncated)
		defer func() { cancel(); <-r.worker.Done() }()
	}
	if cfg.ExecutionConfig != "" {
		if r.worker == nil {
			return fmt.Errorf("execution policy requires search policy")
		}
		r.execution, err = planning.LoadPolicy(cfg.ExecutionConfig)
		r.execution.TokenCheck = r.risks.Check
		r.execution.PoolCheck = r.poolRisks.Check
		if err != nil {
			return err
		}
		r.confirmation = pipeline.NewConfirmationWorker(rpc.WithCaller(ctx, rpc.CallerConfirmation), time.Duration(r.execution.TimeoutMS)*time.Millisecond)
		registry.Sample(telemetry.Standard, telemetry.ConfirmationBacklog, r.confirmation.Backlog)
		defer func() { cancel(); <-r.confirmation.Done() }()
		if cfg.EventSource == "flashblocks" {
			r.flashSnapshots = &flashSnapshotSource{}
			r.flashConfirmation = pipeline.NewConfirmationWorker(rpc.WithCaller(ctx, rpc.CallerConfirmation), time.Duration(r.execution.TimeoutMS)*time.Millisecond)
			registry.Sample(telemetry.Fast, telemetry.ConfirmationBacklog, r.flashConfirmation.Backlog)
			defer func() { cancel(); <-r.flashConfirmation.Done() }()
		}
	}
	r.research, err = pipeline.NewResearch(rpc.WithCaller(ctx, rpc.CallerResearch), cfg.ResearchConfig, cfg.ResearchOut, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()})
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.research.Close()) }()
	r.survival = newRouteSurvival()
	if cfg.ExecutionPaperConfig != "" {
		r.executionPaper, err = newExecutionPaper(rpc.WithCaller(ctx, rpc.CallerPaper), cfg.ExecutionPaperConfig, client, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()}, cfg.Network.DEX, r.research, r.risks, r.poolRisks, r.survival)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, r.executionPaper.Close()) }()
	}
	if cfg.ReactiveConfig != "" {
		if cfg.ReactivePreflight {
			settings, e := loadReactiveSettings(cfg.ReactiveConfig, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()})
			if e != nil {
				return e
			}
			service := &reactiveRuntime{settings: settings, client: client}
			cache, e := service.refresh(rpc.WithCaller(ctx, rpc.CallerReactiveRefresh), r.block, opFeeParams{})
			if e != nil {
				return e
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "preflight_passed", "chain": settings.planning.Chain, "block": cache.block, "signing": false, "submission": false})
		}
		r.reactive, err = newReactiveRuntime(ctx, r)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, r.reactive.close()) }()
	}
	if r.search != nil {
		r.search.Observe = r.research.ObserveSearch
	}
	r.advance()
	r.discovery, err = newPoolDiscovery(rpc.WithCaller(ctx, rpc.CallerDiscovery), r)
	if err != nil {
		return err
	}
	defer r.discovery.close()
	var tickAnswers <-chan tickAnswer
	if cfg.RPCSearchRefresh == "logs" && r.worker != nil {
		r.ticks = newTickRepair(rpc.WithCaller(ctx, rpc.CallerTick))
		defer r.ticks.close()
		tickAnswers = r.ticks.answers
	}
	r.discovery.schedule(r)
	var discoveries <-chan discoveryAnswer
	if r.discovery != nil {
		discoveries = r.discovery.results
	}
	inputCtx, inputCancel := context.WithCancel(ctx)
	updates := make(chan canonicalUpdate)
	streams := make(chan streamEvent, 128)
	registry.Sample(telemetry.Fast, telemetry.FeedBacklog, func() int64 { return int64(len(streams)) })
	wake := make(chan domain.Hash, 1)
	if cfg.EventSource == "sequencer" {
		// Set before any input goroutine starts: the feed writes it and the
		// head subscription and the state writer read it.
		r.feedArrivals = newFeedArrivals()
	}
	failures := make(chan error, 3)
	var inputs sync.WaitGroup
	launch := func(run func() error) {
		inputs.Go(func() {
			if err := run(); err != nil && inputCtx.Err() == nil {
				select {
				case failures <- err:
				case <-inputCtx.Done():
				}
			}
		})
	}
	initialPools := r.store.Pools()
	// A flashblock stream supplies canonical blocks and replaces head hints.
	var seals chan flashSeal
	var flash *flashCanonical
	streamWake := wake
	// On Arbitrum One the Fast Feed's receipts and the standard feed's block
	// hashes do the same when the stream carries whole blocks.
	var sealer *fastSealer
	switch {
	case cfg.EventSource == "flashblocks":
		seals = make(chan flashSeal, 64)
		flash = newFlashCanonical(seals, cfg.Network.Blocks(flashVerifyInterval))
		streamWake = nil
	case cfg.EventSource == "sequencer" && cfg.FastURL != "" && cfg.FastCanonicalFirstIndex == 0:
		seals = make(chan flashSeal, 64)
		flash = newFlashCanonical(seals, cfg.Network.Blocks(flashVerifyInterval))
		sealer = newFastSealer(seals, r.feedArrivals, uint64(cfg.FastFirstIndex), uint64(cfg.FastCanonicalFirstIndex))
	}
	if cfg.RPCHeadsURL != "" && flash == nil {
		launch(func() error {
			rpc.SubscribeHeads(inputCtx, cfg.RPCHeadsURL, func(hash domain.Hash) {
				if at, ok := r.feedArrivals.arrival(hash); ok {
					r.metrics.Observe(telemetry.Standard, telemetry.FeedToHead, time.Since(at))
				}
				offerWake(wake, hash)
			})
			return nil
		})
	}
	launch(func() error {
		return followRPC(rpc.WithCaller(inputCtx, rpc.CallerFollower), cfg, client, specs, block.Ref, radius, wake, flash, updates, initialPools)
	})
	if cfg.EventSource == "sequencer" {
		transport, err := feed.NewClient(cfg.FeedURL, feed.ClientOptions{ChainID: cfg.ChainID(), RequireHeaders: !cfg.Network.FeedHeadersOptional, Compress: cfg.Network.FeedCompression, OnConnection: registry.ConnectionObserver(telemetry.Standard)})
		if err != nil {
			inputCancel()
			inputs.Wait()
			return err
		}
		// ROSETTA_FEED_ARRIVALS_OUT records each message's arrival to compare
		// the public feed with another feed of the same sequences.
		var arrivals *feedArrivalLog
		if path := os.Getenv("ROSETTA_FEED_ARRIVALS_OUT"); path != "" {
			if arrivals, err = openFeedArrivalLog(path); err != nil {
				inputCancel()
				inputs.Wait()
				return err
			}
			defer arrivals.close()
		}
		launch(func() error {
			return transport.Run(inputCtx, func(m feed.Message) error {
				now := time.Now()
				r.feedArrivals.observe(m.BlockHash, m.Sequence, now)
				arrivals.record(m.Sequence, m.BlockHash, now)
				// The feed runs ahead of the RPC node. Its hash lets the follower
				// poll for that block instead of latest; a head subscription
				// announces the block once served, so the feed then stays quiet.
				switch {
				case sealer != nil:
					sealer.standard(m)
				case cfg.RPCHeadsURL == "":
					offerWake(wake, m.BlockHash)
				}
				return nil
			})
		})
	}
	if cfg.EventSource == "flashblocks" {
		launch(func() error {
			return followFlashblocks(rpc.WithCaller(inputCtx, rpc.CallerFlashReceipts), cfg.FlashblocksURL, client, cfg.FlashblocksReceiptsRPC, streams, &r.inputRevision, seals)
		})
	}
	if cfg.FastURL != "" {
		transport, err := feed.NewFastClient(cfg.FastURL, os.Getenv("ROSETTA_FAST_FEED_KEY"), feed.ClientOptions{ChainID: cfg.ChainID(), OnConnection: registry.ConnectionObserver(telemetry.Fast)})
		if err != nil {
			inputCancel()
			inputs.Wait()
			return err
		}
		launch(func() error {
			return transport.Run(inputCtx, func(message feed.FastMessage) error {
				if sealer != nil {
					sealer.fast(message)
				}
				select {
				case streams <- streamEvent{fast: &message, receivedAt: time.Now(), revision: r.inputRevision.Add(1)}:
					return nil
				case <-inputCtx.Done():
					return inputCtx.Err()
				}
			}, func() error {
				if sealer != nil {
					sealer.reset()
				}
				r.inputRevision.Add(1)
				return sendReset(inputCtx, streams)
			})
		})
	}
	defer func() { inputCancel(); inputs.Wait() }()
	var searches <-chan arb.SearchEvent
	var diagnoses <-chan routeDiagnosis
	if r.reactive != nil {
		diagnoses = r.reactive.diagnoses
	}
	if r.worker != nil {
		searches = r.worker.Results()
	}
	var confirmations <-chan pipeline.ConfirmationEvent
	if r.confirmation != nil {
		confirmations = r.confirmation.Results()
	}
	var flashConfirmations <-chan pipeline.ConfirmationEvent
	if r.flashConfirmation != nil {
		flashConfirmations = r.flashConfirmation.Results()
	}
	rotations := control.Rotations(ctx)
	beat := telemetry.HeartbeatFrom(ctx)
	var paperTicks <-chan time.Time
	// The timer also keeps an idle loop beating for the watchdog.
	if r.executionPaper != nil || r.reactive != nil || beat != nil {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		paperTicks = ticker.C
	}
	// Shutdown can surface as an error from any handler, not only at the
	// select below. The canonical store changes only by whole committed blocks,
	// so it is consistent on every exit; persist it so the next start does not
	// replay the blocks since an older snapshot.
	defer func() { result = r.persistOnShutdown(ctx, result) }()
	accepted := 0
	streamBurst := 0
	for {
		beat.Beat(time.Now())
		// Prefer ready feed frames, then return to the fair select so canonical
		// reconciliation, confirmations and background adoption keep making progress.
		if streamBurst < 4 {
			select {
			case event := <-streams:
				streamBurst++
				if err := r.acceptStreamEvent(ctx, event, streamWake); err != nil {
					return err
				}
				continue
			default:
			}
		}
		streamBurst = 0
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), r.writeSnapshot())
		case now := <-paperTicks:
			r.checkSilence(now)
			if r.reactive != nil {
				if e := r.reactive.failure.Load(); e != nil {
					return *e
				}
				if r.reactiveRefreshDue() {
					r.reactive.schedule(r.block, r.feeParams)
				}
				if r.reactive.quiet != nil && r.reactive.quiet.warmDue(now) {
					r.reactive.quiet.warm(r.store.Pools())
				}
			}
			if r.executionPaper == nil {
				continue
			}
			r.scheduleReactiveVerification()
			r.executionPaper.flushSelection(now, false)
			r.executionPaper.expireAll(now)
			if err := r.executionPaper.error(); err != nil {
				return err
			}
		case err := <-failures:
			return errors.Join(err, r.writeSnapshot())
		case request := <-rotations:
			r.rotate(ctx, request)
		case update := <-updates:
			err := r.acceptCanonical(ctx, update)
			close(update.accepted)
			if err != nil {
				return err
			}
			if update.reset == nil {
				accepted++
			}
			if cfg.Blocks > 0 && accepted >= cfg.Blocks {
				inputCancel()
				inputs.Wait()
				if err := r.discovery.flush(rpc.WithCaller(ctx, rpc.CallerDiscovery), r); err != nil {
					return err
				}
				if err := r.flush(ctx, searches, confirmations); err != nil {
					return err
				}
				return r.writeSnapshot()
			}
		case event := <-streams:
			if err := r.acceptStreamEvent(ctx, event, streamWake); err != nil {
				return err
			}
		case answer := <-discoveries:
			if err := r.discovery.consume(rpc.WithCaller(ctx, rpc.CallerDiscovery), r, answer); err != nil {
				return err
			}
		case request := <-diagnoses:
			r.diagnoseRoute(ctx, request)
		case answer := <-tickAnswers:
			if err := r.ticks.consume(ctx, r, answer); err != nil {
				return err
			}
			r.ticks.schedule(ctx, r)
		case event := <-searches:
			if err := r.search.Process(ctx, event, r, nil, nil, r); err != nil {
				return err
			}
		case event := <-flashConfirmations:
			if err := r.consumeFlashConfirmation(ctx, event); err != nil {
				return err
			}
		case event := <-confirmations:
			if err := r.consumeConfirmation(ctx, event); err != nil {
				return err
			}
		}
	}
}

// reactiveRefreshDue reports whether the canonical head is worth a reactive
// context read. Admission skips catch-up, and a replayed block can predate the
// configured executor, so replaying far behind the tip reads nothing.
func (r *runtime) reactiveRefreshDue() bool {
	return !r.catchingUp || r.nearTip
}

// topologyOptions is the one place topology limits come from: every build,
// reset rebuild and discovery extension used to spell them out, and two had
// dropped the fast lane's per-pool cap.
func (r *runtime) topologyOptions() pipeline.TopologyOptions {
	return pipeline.TopologyOptions{TokenCheck: r.risks.Check, PoolCheck: r.poolRisks.Check, Depth: r.policy.Depth, MaxCycles: r.policy.MaxCycles,
		K: r.policy.K, ExplorePaths: r.policy.ExplorePaths, FastCoarseCycles: r.policy.FastCoarseCycles, FastLaneHops: r.policy.FastLaneHops, FastLaneQuotes: r.policy.FastLaneQuotes, Notional: r.policy.Notional}
}
