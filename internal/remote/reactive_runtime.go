package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/bidmodel"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
	"github.com/gofrs/flock"
)

// reactiveSkipReport is how often skip totals are written to the output.
const reactiveSkipReport = time.Minute

// A context refresh that the RPC rejects is retried this often, this far
// apart: about the time the RPC takes to serve a block the stream sealed.
const (
	reactiveRefreshRetries    = 3
	reactiveRefreshRetryDelay = 200 * time.Millisecond
)

type reactiveJob struct {
	timing       reactiveTiming
	intent       execution.ReactiveIntent
	current      func() bool
	cancellation *execution.Cancellation
	target       domain.Hash
	// superseded is set by offerJob when a better job replaces this one;
	// current then fails. taken marks a job a lane is running; queueMu.
	superseded *atomic.Bool
	taken      bool
	// group names one opportunity's variants raced on several lanes, whose
	// conditions void all but the first to execute; variant marks the
	// hedges, which only take idle lanes.
	group   uint64
	variant bool
	// verify replays the intent at its quote state once it is admitted or sent.
	verify *paperExecutionJob
}
type reactiveRefresh struct {
	block     rpc.ExecutionBlock
	feeParams opFeeParams
}
type reactiveRuntime struct {
	trees     bidmodel.Store
	treesDone chan struct{}

	gate        *control.Gate
	metrics     *telemetry.Registry
	research    *pipeline.Research
	refreshDone chan struct{}
	settings    reactiveSettings
	// lenders follows V3 flash lenders' balances of the funding assets.
	lenders *lenderBook
	// accountBlocks is reactiveAccountInterval in blocks of this chain.
	accountBlocks uint64
	client        *rpc.Client
	cache         atomic.Pointer[reactiveCache]
	jobs          chan struct{} // the primary lane's job signal
	queueMu       sync.Mutex
	slots         []*reactiveJob // queued and running jobs, at most one per lane
	groups        atomic.Uint64  // the last variant group

	// extra are the send lanes after the primary; results carries
	// every lane's job records to the writer.
	extra        []*sendLane
	results      chan map[string]any
	workers      sync.WaitGroup
	laneReported map[int]*execution.DispatchStatus // extra lanes' last dispatch_state; writer only

	poolMu       sync.Mutex // poolVersions: frames touch it, the build worker guards on it
	poolVersions map[domain.PoolKey]*atomic.Uint64
	offerMu      sync.Mutex
	offerSlot    *reactiveOffer // latest admitted search result not yet built
	tail         longTail       // main loop only
	// fastSlot holds the latest fast-lane offer. It is built before offerSlot:
	// the full search admits a new result tens of times a second, and in one
	// slot each one replaced a waiting fast-lane offer before it was built.
	fastSlot *reactiveOffer
	// diagnoses carries admitted full-search routes the fast lane did not
	// quote from the build worker to the main loop (routeDiagnosis).
	diagnoses  chan routeDiagnosis
	offerReady chan struct{}
	buildDone  chan struct{}
	applied    atomic.Uint64
	// head is the latest applied canonical block number (quietHead).
	head         atomic.Uint64
	updates      chan reactiveRefresh
	dispatcher   *execution.Dispatcher
	cancel       context.CancelFunc
	done         chan struct{}
	failure      atomic.Pointer[error]
	observations chan any
	// diagnostics carries records the writer may lose (note); observations
	// carries execution evidence, whose loss stops the instance.
	diagnostics  chan any
	lastObserved *execution.ExecutionObservation // state writer only
	lastRefresh  domain.Hash                     // main runtime goroutine only
	// verifiedConfig is the executor configVersion whose configuration passed
	// verifyExecutor; reads holds the last refresh's chain reads. Startup and
	// refreshLoop own both in turn.
	verifiedConfig uint64
	reads          *reactiveReads
	// accountStale asks the next refresh to reread balances and the executor
	// version: one of our transactions landed, so the wallet changed.
	accountStale atomic.Bool
	skipMu       sync.Mutex
	skips        map[string]uint64
	// verification is the latest reactive intent awaiting the paper worker. The
	// send goroutine offers it; the main loop takes it.
	verifyMu     sync.Mutex
	verification *paperExecutionJob
	competition  *competitionObserver      // rival bids seen in the feed; nil only in tests
	bids         *bidEstimator             // nil when bid_model is off
	archive      *bidArchive               // nil unless bid_archive is set
	bidsDone     chan struct{}             // closed after the last bid history save
	survival     *routeSurvival            // shared with execution paper, which refines its prior
	own          ownFlight                 // our sends until the quote view includes them
	backlog      sequencerBacklog          // holds sends after a slow Nitro sequencer answer
	preflight    *reactivePreflight        // nil unless preflight_simulation
	quiet        *quietPools               // nil unless hot_path or packed_conditions
	assets       []domain.Token            // start asset units for the console
	reported     *execution.DispatchStatus // last dispatch_state written; writer only
	refillAfter  time.Time                 // next native_refill check; refresh goroutine only
}

func newReactiveRuntime(ctx context.Context, r *runtime) (*reactiveRuntime, error) {
	settings, err := loadReactiveSettings(r.cfg.ReactiveConfig, domain.ChainIdentity{Family: r.cfg.Network.Family, ChainID: r.cfg.ChainID()})
	if err != nil {
		return nil, err
	}
	if settings.document.Mode == "live" && r.cfg.TokenRiskFile == "" {
		return nil, fmt.Errorf("live execution requires -token-risk-file")
	}
	settings.planning.Ordering = r.cfg.Network.Ordering
	accountBlocks := r.cfg.Network.Blocks(reactiveAccountInterval)
	settings.planning.TokenCheck = r.risks.Check
	settings.planning.PoolCheck = r.poolRisks.Check
	var assets []domain.Address
	for token := range settings.minimum {
		assets = append(assets, token)
	}
	lenders := newLenderBook(assets)
	settings.planning.LenderBalance = lenders.balance
	settings.limits.TokenCheck = r.risks.Check
	settings.limits.PoolCheck = r.poolRisks.Check
	child, cancel := context.WithCancel(ctx)
	s := &reactiveRuntime{lenders: lenders, accountBlocks: accountBlocks, gate: r.gate, metrics: r.metrics, settings: settings, client: r.client, research: r.research, observations: make(chan any, 64), diagnostics: make(chan any, 64), jobs: make(chan struct{}, 1), updates: make(chan reactiveRefresh, 1), cancel: cancel, done: make(chan struct{})}
	for i, lane := range settings.lanes {
		s.extra = append(s.extra, &sendLane{index: i + 1, sender: lane.sender, notify: make(chan struct{}, 1)})
	}
	s.results = make(chan map[string]any, reactiveResultQueue)
	s.laneReported = map[int]*execution.DispatchStatus{}
	fail := func(e error) (*reactiveRuntime, error) { cancel(); return nil, e }
	cache, err := s.startContext(rpc.WithCaller(child, rpc.CallerReactiveRefresh), r.block, r.feeParams)
	if err != nil {
		return fail(err)
	}
	s.cache.Store(cache)
	s.metrics.Set(telemetry.Standard, telemetry.WalletNative, gwei(cache.native))
	s.lastRefresh = r.block.Ref.Hash
	s.assets = s.readAssets(rpc.WithCaller(child, rpc.CallerBootstrap), r.block)
	if settings.planning.HotPath || settings.planning.PackedConditions {
		// A failed refresh clears the cache; with no context block no pool is
		// quiet (conditions) or read (fetch).
		s.quiet = newQuietPools(r.client, s.quietHead)
		s.quiet.note = s.skip
		s.quiet.store = quietStorePath(settings.document.Output)
		s.quiet.load()
		s.settings.planning.QuietPool = s.quiet.conditions
		go s.quiet.run(rpc.WithCaller(child, rpc.CallerReactiveRefresh))
	}
	r.research.SetWrappedNative(settings.wrapped)
	protected := map[domain.Address]bool{settings.wrapped: true}
	for token := range settings.minimum {
		protected[token] = true
	}
	if settings.document.PreflightSimulation {
		var trace *rpc.Client
		if endpoint := os.Getenv("ROSETTA_TRACE_RPC_URL"); endpoint != "" {
			trace = rpc.NewClient(endpoint, rpc.Options{Timeout: 5 * time.Second})
		}
		// The bid estimator is built below; measurements arrive only after
		// the runtime starts building intents.
		routeGas := func(route []domain.PoolKey, funding execution.Funding, used uint64) {
			if s.bids != nil {
				s.bids.observeRouteGas(route, funding, used)
			}
		}
		s.preflight = newReactivePreflight(rpc.WithCaller(child, rpc.CallerSettlement), r.client, trace, r.risks, r.poolRisks, protected, preflightStorePath(settings.document.Output), settings.limits.Executor, routeGas)
		s.settings.planning.RouteRan = s.preflight.ran
		s.preflight.unprovenCover = settings.document.UnprovenRouteCoverBPS
	}
	if settings.document.Mode == "live" {
		endpoint := r.cfg.SubmitURL
		if endpoint == "" {
			endpoint = os.Getenv("ROSETTA_SUBMIT_RPC_URL")
		}
		if endpoint == "" {
			return fail(fmt.Errorf("live reactive mode requires -submit-rpc or ROSETTA_SUBMIT_RPC_URL"))
		}
		signer, e := execution.LoadSecretSigner(child, os.Getenv("ROSETTA_SIGNING_SECRET_ID"), settings.planning.Sender)
		if e != nil {
			return fail(e)
		}
		submit, e := openSubmitClient(child, endpoint, "", settings.planning.Chain.ChainID, submitWarmInterval)
		if e != nil {
			return fail(e)
		}
		network := &execution.RPCDispatchNetwork{TokenRisks: r.risks, ProtectedTokens: protected, Read: r.client, Submit: submit, Chain: settings.planning.Chain}
		var logPaths func(domain.Hash, []execution.PathResult)
		network.Fanout, logPaths = openFanout(child, os.Getenv("ROSETTA_SUBMIT_FANOUT_RPC_URLS"), settings.planning.Chain.ChainID)
		network.OnPaths = s.pathRecorder(logPaths)
		if endpoint := os.Getenv("OP_EXECUTION_RPC_URL"); endpoint != "" && settings.planning.Chain.Family == domain.FamilyOPStack {
			network.Trace = rpc.NewClient(endpoint, rpc.Options{Timeout: 3 * time.Second})
		} else if endpoint := os.Getenv("ROSETTA_TRACE_RPC_URL"); endpoint != "" {
			network.Trace = rpc.NewClient(endpoint, rpc.Options{Timeout: 5 * time.Second})
		}
		s.dispatcher, e = execution.OpenDispatcher(rpc.WithCaller(child, rpc.CallerSettlement), settings.document.Journal, settings.limits, signer, network)
		if e != nil {
			return fail(e)
		}
		for i, lane := range settings.lanes {
			laneSigner, e := execution.LoadSecretSigner(child, lane.secretID, lane.sender)
			if e != nil {
				s.closeLanes()
				return fail(e)
			}
			limits := lane.limits
			limits.TokenCheck, limits.PoolCheck = r.risks.Check, r.poolRisks.Check
			if s.extra[i].dispatcher, e = execution.OpenDispatcher(rpc.WithCaller(child, rpc.CallerSettlement), lane.journal, limits, laneSigner, network); e != nil {
				s.closeLanes()
				return fail(e)
			}
		}
	}
	if err = os.MkdirAll(filepath.Dir(settings.document.Output), 0700); err != nil {
		s.closeLanes()
		return fail(err)
	}
	lock := flock.New(settings.document.Output + ".lock")
	ok, err := lock.TryLock()
	if err != nil || !ok {
		s.closeLanes()
		return fail(fmt.Errorf("reactive output already owned"))
	}
	f, err := os.OpenFile(settings.document.Output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		lock.Unlock()
		s.closeLanes()
		return fail(err)
	}
	s.survival = r.survival
	var onRival func(competitorRecord)
	if settings.document.BidModel != "off" {
		s.bids = newBidEstimator(time.Duration(settings.document.BidLandingMS)*time.Millisecond, time.Duration(settings.document.BidWindowMS)*time.Millisecond)
		s.bids.gasPrior = float64(settings.document.GasPriorBPS) / 10000
		s.bids.load(bidStorePath(settings.document.Output), time.Now())
		if settings.document.BidArchive {
			if s.archive, err = openBidArchive(child, bidArchivePath(settings.document.Output), bidArchiveMaxBytes); err != nil {
				lock.Unlock()
				f.Close()
				s.closeLanes()
				return fail(err)
			}
			s.bids.archive = s.archive.window
		}
		s.settings.planning.Bid = s.bidDecision
		if settings.document.TipOnExpectedGas {
			s.settings.planning.TipGas = s.bids.routeExpectedGas
		}
		onRival = func(record competitorRecord) {
			sighting := rivalSighting{pools: record.Pools, from: record.From, tip: rivalTip(record.Tip), block: record.Block, frame: record.Frame, single: len(record.Pools) < 2}
			if record.To != nil {
				sighting.to = *record.To
			}
			s.bids.observe(sighting, time.Unix(0, record.ReceivedAtNS))
		}
	}
	if s.competition, err = openCompetitionObserver(child, settings.document.Output+".competition.jsonl", settings.planning.Chain.ChainID, s.senders(), onRival); err != nil {
		lock.Unlock()
		f.Close()
		s.closeLanes()
		return fail(err)
	}
	if err := control.AnnounceOutputs(ctx, control.Outputs{Reactive: settings.document.Output, Competition: settings.document.Output + ".competition.jsonl"}); err != nil {
		slog.Warn("reactive outputs not announced to the console", "error", err)
	}
	if settings.document.BidTreeMode != "off" {
		scope := s.treeScope()
		if err := s.trees.Load(settings.document.BidTreeFile, scope, time.Now()); err != nil {
			slog.Warn("bid tree fallback", "error", err)
		}
		s.treesDone = make(chan struct{})
		go func() {
			defer close(s.treesDone)
			s.trees.Watch(child, settings.document.BidTreeFile, scope, func(err error) {
				if err != nil {
					slog.Warn("bid tree reload retained previous model", "error", err)
				}
			})
		}()
	}
	if s.bids != nil {
		s.bidsDone = make(chan struct{})
		go s.bids.persist(child, bidStorePath(settings.document.Output), s.bidsDone)
	}
	s.refreshDone = make(chan struct{})
	go s.refreshLoop(rpc.WithCaller(child, rpc.CallerReactiveRefresh))
	s.offerReady, s.buildDone = make(chan struct{}, 1), make(chan struct{})
	s.diagnoses = make(chan routeDiagnosis, 16)
	go s.buildLoop(child, r)
	go s.run(child, f, lock)
	return s, nil
}
func (s *reactiveRuntime) close() error {
	if s == nil {
		return nil
	}
	s.cancel()
	<-s.done
	<-s.refreshDone
	if s.buildDone != nil {
		<-s.buildDone
	}
	if s.competition != nil {
		<-s.competition.done
	}
	if s.bidsDone != nil {
		<-s.bidsDone
	}
	if s.treesDone != nil {
		<-s.treesDone
	}
	if s.preflight != nil {
		<-s.preflight.done
	}
	if s.archive != nil {
		<-s.archive.done
	}
	if e := s.failure.Load(); e != nil {
		return *e
	}
	return nil
}
func (s *reactiveRuntime) schedule(block rpc.ExecutionBlock, feeParams opFeeParams) {
	if s == nil || block.Ref.Hash == s.lastRefresh {
		return
	}
	select {
	case s.updates <- reactiveRefresh{block: block, feeParams: feeParams}:
		s.lastRefresh = block.Ref.Hash
	default:
	}
}
func (s *reactiveRuntime) run(ctx context.Context, file *os.File, lock *flock.Flock) {
	defer close(s.done)
	defer file.Close()
	defer lock.Unlock()
	encoder := json.NewEncoder(file)
	write := func(value any) bool {
		err := encoder.Encode(value)
		if err == nil {
			err = file.Sync()
		}
		if err != nil {
			s.failure.Store(&err)
			return false
		}
		return true
	}
	lanes := s.allLanes()
	stop := make(chan struct{})
	for _, lane := range lanes {
		s.workers.Add(1)
		go s.sendLoop(ctx, stop, lane)
	}
	// Every lane finishes its send, and its record is written, before the
	// journals close.
	defer s.closeLanes()
	defer func() {
		close(stop)
		finished := make(chan struct{})
		go func() { s.workers.Wait(); close(finished) }()
		for {
			select {
			case result := <-s.results:
				write(result)
			case <-finished:
				return
			}
		}
	}()
	if len(s.assets) > 0 && !write(map[string]any{"type": "reactive_assets", "at_ms": time.Now().UnixMilli(), "assets": s.assets}) {
		return
	}
	followups := s.forwardFollowups(stop, lanes)
	for _, lane := range lanes {
		if lane.dispatcher == nil {
			continue
		}
		if previous := lane.dispatcher.LastSettlement(); previous != nil {
			if !write(map[string]any{"type": "settlement", "recovered": true, "observed_at_ns": time.Now().UnixNano(), "receipt": previous}) {
				return
			}
		}
	}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	reported := time.Now()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case observation := <-s.observations:
					if !s.record(observation, write) {
						return
					}
				default:
					return
				}
			}
		case observation := <-s.observations:
			if !s.record(observation, write) {
				return
			}
		case diagnostic := <-s.diagnostics:
			if !s.record(diagnostic, write) {
				return
			}
		case f := <-followups:
			updates, err := f.lane.dispatcher.ApplyFollowup(f.followup, time.Now())
			if !s.publishSettlements(updates, err, write) {
				return
			}
		case result := <-s.results:
			if !write(result) || s.failure.Load() != nil {
				return
			}
		case now := <-timer.C:
			if now.Sub(reported) >= reactiveSkipReport {
				if skips := s.takeSkips(); len(skips) > 0 {
					if survival := s.survival.snapshot(); len(survival) > 0 && !write(map[string]any{"type": "route_survival", "at_ms": now.UnixMilli(), "survived_of_total_by_hops": survival}) {
						return
					}
					if !write(map[string]any{"type": "reactive_skips", "from_ms": reported.UnixMilli(), "to_ms": now.UnixMilli(), "reasons": skips}) {
						return
					}
				}
				reported = now
			}
			for _, lane := range lanes {
				if lane.dispatcher == nil {
					continue
				}
				if !s.publishDispatch(now, lane, write) {
					return
				}
				// Canonical sightings and due checks; any RPC runs in dispatcher tasks.
				updates, err := lane.dispatcher.Watch(now)
				if !s.publishSettlements(updates, err, write) {
					return
				}
			}
		}
	}
}

// reactiveResultQueue holds intent records between the send lanes and the
// writer. Handing a record straight to the writer would make the lane wait
// behind its writes, each with an fsync, adding milliseconds at p99 to the
// time from seal to send. The writer falls this far behind only if the
// disk stalls; a lane then waits rather than lose a record of something it
// sent.
const reactiveResultQueue = 256

// record writes one queued record. A feed sighting of our transaction is
// booked at once: the outcome is final unless a canonical block disagrees.
// Only the lane that sent it books it.
func (s *reactiveRuntime) record(value any, write func(any) bool) bool {
	if !write(value) {
		return false
	}
	observation, ok := value.(*execution.ExecutionObservation)
	if !ok {
		return true
	}
	for _, lane := range s.allLanes() {
		if lane.dispatcher == nil {
			continue
		}
		updates, err := lane.dispatcher.SettleObserved(observation, time.Now())
		if !s.publishSettlements(updates, err, write) {
			return false
		}
	}
	return true
}

func (s *reactiveRuntime) refreshLoop(ctx context.Context) {
	defer close(s.refreshDone)
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-s.updates:
			started := time.Now()
			cache, err := s.refresh(ctx, update.block, update.feeParams)
			// The follower can take a block from the flashblock stream before
			// the RPC serves it, which answers with a JSON-RPC error. Retry while
			// no newer block is queued; other failures are not retried.
			var unavailable *rpc.RPCError
			for attempt := 0; errors.As(err, &unavailable) && attempt < reactiveRefreshRetries && len(s.updates) == 0; attempt++ {
				timer := time.NewTimer(reactiveRefreshRetryDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				cache, err = s.refresh(ctx, update.block, update.feeParams)
			}
			s.metrics.Since(telemetry.Standard, telemetry.ReactiveRefresh, started)
			s.seedLenders(ctx, update.block)
			s.refill(ctx, time.Now())
			if err != nil {
				s.cache.Store(nil)
				slog.Warn("reactive context refresh failed", "error", err)
				continue
			}
			s.cache.Store(cache)
			s.metrics.Set(telemetry.Standard, telemetry.WalletNative, gwei(cache.native))
		}
	}
}

// seedLenders reads every lender balance at block when the book has none. A
// failed or superseded read is retried at the next refresh.
func (s *reactiveRuntime) seedLenders(ctx context.Context, block rpc.ExecutionBlock) {
	slots := s.lenders.needsSeed()
	if len(slots) == 0 {
		return
	}
	reader := &snapshot{client: s.client, block: block, chain: s.settings.planning.Chain}
	defer reader.Close()
	calls := make([]rpc.Call, len(slots))
	for i, slot := range slots {
		calls[i] = rpc.Call{Method: "eth_call", Params: reader.contractParams(slot.token, reactiveData("balanceOf(address)", slot.pool[:]), nil)}
	}
	results, err := reader.batchStrings(ctx, calls)
	if err != nil {
		slog.Warn("flash lender balances not read", "lenders", len(slots), "error", err)
		return
	}
	values := make([]*big.Int, len(results))
	for i, result := range results {
		if values[i], err = integer(result); err != nil {
			slog.Warn("flash lender balance malformed", "pool", slots[i].pool.Hex(), "error", err)
			return
		}
	}
	if !s.lenders.install(block.Ref, slots, values) {
		slog.Info("flash lender balances superseded before install", "block", block.Ref.Number)
		return
	}
	slog.Info("flash lender balances seeded", "slots", len(slots), "block", block.Ref.Number)
}

// queueOffer keeps the latest admitted result per lane: a newer frame
// supersedes an older one that the worker has not started.
func (s *reactiveRuntime) queueOffer(offer *reactiveOffer) {
	s.offerMu.Lock()
	slot := &s.offerSlot
	if offer.timing.Lane == "fast" {
		slot = &s.fastSlot
	}
	if *slot != nil {
		s.skip("offer_superseded")
		s.skip("offer_superseded_" + laneName(offer.timing.Lane))
	}
	*slot = offer
	s.offerMu.Unlock()
	s.signalOffer()
}

func (s *reactiveRuntime) signalOffer() {
	select {
	case s.offerReady <- struct{}{}:
	default:
	}
}

// takeOffer returns the next offer to build, the fast lane's first.
func (s *reactiveRuntime) takeOffer() *reactiveOffer {
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	offer := s.fastSlot
	if offer != nil {
		s.fastSlot = nil
	} else {
		offer, s.offerSlot = s.offerSlot, nil
	}
	if s.fastSlot != nil || s.offerSlot != nil {
		s.signalOffer()
	}
	return offer
}

func laneName(lane string) string {
	if lane == "" {
		return "worker"
	}
	return lane
}

func (s *reactiveRuntime) buildLoop(ctx context.Context, r *runtime) {
	defer close(s.buildDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.offerReady:
			if offer := s.takeOffer(); offer != nil {
				r.buildReactive(ctx, offer)
			}
		}
	}
}

// quietHead is the block the quiet-pool reader measures quietness from: the
// latest applied canonical block. The context cache is refreshed only a few
// times a minute and stands in until a block is applied; with no context
// (a failed refresh) no pool is quiet or read.
func (s *reactiveRuntime) quietHead() uint64 {
	cache := s.cache.Load()
	if cache == nil {
		return 0
	}
	return max(s.head.Load(), cache.block.Number)
}
