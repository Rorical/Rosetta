package remote

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// reactiveContextLag bounds how far the canonical follower, and the reactive
// context refreshed from it, may trail a Fast anchor; the runtime converts it
// to blocks of its chain. OP canonical reads run about two blocks behind the
// Fast head in steady state. The fee cap reserves twice the cached base fee;
// OP Mainnet's EIP-1559 parameters (denominator 250, elasticity 2) allow at
// most 0.4% growth per block, about 8% over twenty blocks (40 s), well inside
// that headroom. Nitro prices congestion per second, not per block; that 40 s
// stays inside the headroom there is UNVERIFIED. Pool state comes from the
// Fast branch, not this context.
const reactiveContextLag = 40 * time.Second

// reactiveBuildCandidates bounds the expensive per-opportunity work. One nonce
// lane sends at most one route, so only the highest quoted gross candidates are
// worth repricing at smaller principals and tip-dependent fees.
const reactiveBuildCandidates = 3

// reactiveBuildAttempts bounds the candidates one build looks at. Candidates
// rank by gross and route length, not by what they cost: one worth less than
// its cheapest network cost is dropped before it is counted, and one that
// seals no intent gives its slot back.
const reactiveBuildAttempts = 16

// reactiveOffer is a search result admitted on the main loop and built on the
// reactive worker. It captures everything the build reads from state owned by
// the main goroutine.
type reactiveOffer struct {
	event                arb.SearchEvent
	engine               *arb.Engine
	cache                *reactiveCache
	pending              *execution.PendingTransaction
	revision, generation uint64
	timing               reactiveTiming
	lenders              []pool.State // standalone V3 flash lenders at the quoted state
	// verify binds the quote state for a replay of the built intent; nil without paper.
	verify *paperExecutionJob
	// fastGap classifies a full-search offer's leading routes by how the fast
	// lane covered them for the same frame (fastCoverage).
	fastGap map[string]string
	// routeOrigin says whether each leading route came from the precomputed
	// table or from background exploration (routeOrigins).
	routeOrigin map[string]string
}

// reactiveTiming is written with every intent record. QuotedNS is the end of
// exact search; the main loop picks the result up at ConsumedNS.
type reactiveTiming struct {
	ReceivedNS   int64 `json:"received_ns"`
	QuotedNS     int64 `json:"quoted_ns"`
	ConsumedNS   int64 `json:"consumed_ns"`
	BuildStartNS int64 `json:"build_start_ns"`
	SealedNS     int64 `json:"sealed_ns"`
	ExecuteNS    int64 `json:"execute_ns"`
	// FeedNS is when the standard feed delivered the quote block, before the
	// local node served it; zero without a sequencer feed.
	FeedNS int64  `json:"feed_ns,omitempty"`
	Lane   string `json:"lane,omitempty"`
	// DequeuedNS and LaneStartNS split a fast-lane quote into waiting for the
	// main loop, main-loop work before quoting, and quoting (to QuotedNS).
	DequeuedNS  int64 `json:"dequeued_ns,omitempty"`
	LaneStartNS int64 `json:"lane_start_ns,omitempty"`
	// Frame splits a fast-lane intent's main-loop work before quoting; Build
	// splits the build. Both are nanoseconds, kept to find stalls.
	Frame *frameSteps `json:"frame,omitempty"`
	Build *buildSteps `json:"build,omitempty"`
}

// frameSteps times the main loop's work on the frame a fast-lane quote follows.
type frameSteps struct {
	Apply    int64 `json:"apply,omitempty"`
	Observe  int64 `json:"observe,omitempty"`
	View     int64 `json:"view,omitempty"`
	Research int64 `json:"research,omitempty"`
	Advance  int64 `json:"advance,omitempty"`
	Paper    int64 `json:"paper,omitempty"`
	Cancel   int64 `json:"cancel,omitempty"`
	Submit   int64 `json:"submit,omitempty"`
}

// buildSteps times the parts of one reactive build.
type buildSteps struct {
	Rank    int64 `json:"rank,omitempty"`
	Native  int64 `json:"native,omitempty"`
	Requote int64 `json:"requote,omitempty"`
	Plan    int64 `json:"plan,omitempty"`
	Seal    int64 `json:"seal,omitempty"`
	Plans   int   `json:"plans,omitempty"`
}

// offerReactive admits and builds synchronously; queueReactive hands the build
// to the reactive worker so the main loop is not held by repricing.
func (r *runtime) offerReactive(ctx context.Context, event arb.SearchEvent) error {
	offer, err := r.admitReactive(event)
	if err != nil || offer == nil {
		return err
	}
	r.buildReactive(ctx, offer)
	return nil
}

func (r *runtime) queueReactive(event arb.SearchEvent) error {
	return r.queueReactiveLane(event, "", time.Time{}, time.Time{}, nil)
}

// queueReactiveLane tags the offer with the path that produced it, so intent
// timelines from the fast lane and the full search can be compared. dequeued
// and started are the fast lane's main-loop pickup and quote start; zero
// leaves them out of the record.
// fastCoverage classifies the leading routes of a full-search result on a
// Fast version: whether the fast lane held them, ranked them for the frame,
// and quoted them. It runs on the main loop, which owns the fast-lane engine.
func (r *runtime) fastCoverage(event arb.SearchEvent) map[string]string {
	if event.Job.Version.Source != domain.SourceFast || r.topology == nil || r.topology.Fast == nil {
		return nil
	}
	now := time.Now()
	gap := make(map[string]string, reactiveBuildCandidates)
	for _, o := range event.Result.Opportunities[:min(len(event.Result.Opportunities), reactiveBuildCandidates)] {
		key := arb.RouteKey(o.Cycle)
		class := r.topology.Fast.FastCoverage(o.Cycle, event.Job.Dirty, now)
		if class == arb.CoverageCandidate {
			// A reconciled canonical block searches every pool without the
			// fast lane, which then has no quote of this version at all.
			_, quoted := r.fastQuoted.routes[key]
			switch {
			case r.fastQuoted.version != event.Job.Version:
				class = "fast_not_run_" + r.frameHistory(o.Cycle, key, event.Job.Version.BlockNumber)
			case quoted:
				class = "fast_quoted"
			default:
				// Selected by the frame's coarse rank and then not published,
				// or never selected: exact sizing against coarse ranking.
				class = "not_quoted_unselected"
				if _, selected := r.fastQuoted.selected[key]; selected {
					class = "not_quoted_selected"
				}
			}
		}
		gap[key] = class
	}
	return gap
}

// routeOrigins labels a full-search result's leading routes "table" when the
// precomputed table holds them and "explored" when only background
// exploration found them, to weigh the explore_paths cache.
func (r *runtime) routeOrigins(event arb.SearchEvent) map[string]string {
	if r.topology == nil || r.topology.Engine == nil || event.Job.View == nil {
		return nil
	}
	origins := make(map[string]string, reactiveBuildCandidates)
	for _, o := range event.Result.Opportunities[:min(len(event.Result.Opportunities), reactiveBuildCandidates)] {
		origin := "explored"
		if r.topology.Engine.TableHolds(event.Job.View, o.Cycle) {
			origin = "table"
		}
		origins[arb.RouteKey(o.Cycle)] = origin
	}
	return origins
}

// routeDiagnosis asks the main loop, which owns the fast-lane engine, why the
// fast lane did not quote a route the full search admitted.
type routeDiagnosis struct {
	view  arb.View
	cycle graph.Cycle
	dirty []domain.PoolKey
	class string
}

func (r *runtime) diagnoseRoute(ctx context.Context, request routeDiagnosis) {
	if r.topology == nil || r.topology.Fast == nil {
		return
	}
	dirty := request.dirty
	if !strings.HasPrefix(request.class, "not_quoted") {
		// A reconciled block marks every pool dirty; ranking them all here
		// would quote the whole table on the main loop.
		dirty = request.cycle.Pools
	}
	diagnosis, err := r.topology.Fast.DiagnoseRoute(ctx, request.view, request.cycle, dirty, r.searchOptions)
	if err != nil {
		diagnosis = "error"
	}
	r.reactive.skip("fast_diag_" + request.class + "_" + diagnosis)
}

// frameHistory says what the frames of a version's block and the one before
// it did to a route: none changed its pools (clean), some did but the fast
// lane never quoted it (dirty_not_quoted), or the fast lane quoted it
// (dirty_quoted). A reconciled view can already hold the next block's frames.
func (r *runtime) frameHistory(cycle graph.Cycle, key string, number uint64) string {
	class := "clean"
	for _, n := range []uint64{number, number - 1} {
		block := r.blockFrames[n]
		if block == nil {
			continue
		}
		if _, quoted := block.quoted[key]; quoted {
			return "dirty_quoted"
		}
		for _, pool := range cycle.Pools {
			if _, changed := block.dirty[pool]; changed {
				class = "dirty_not_quoted"
			}
		}
	}
	return class
}

func (r *runtime) queueReactiveLane(event arb.SearchEvent, lane string, dequeued, started time.Time, frame *frameSteps) error {
	offer, err := r.admitReactive(event)
	if err != nil || offer == nil {
		return err
	}
	offer.timing.Lane = lane
	if !dequeued.IsZero() {
		offer.timing.DequeuedNS = dequeued.UnixNano()
	}
	if !started.IsZero() {
		offer.timing.LaneStartNS = started.UnixNano()
	}
	offer.timing.Frame = frame
	if lane == "" {
		offer.fastGap = r.fastCoverage(event)
		offer.routeOrigin = r.routeOrigins(event)
	}
	r.reactive.queueOffer(offer)
	return nil
}

// admitReactive runs on the main loop. It applies the cheap gates that read
// main-goroutine state and captures what the build needs.
func (r *runtime) admitReactive(event arb.SearchEvent) (*reactiveOffer, error) {
	consumed := time.Now()
	revision := r.inputRevision.Load()
	generation := r.reactiveRevision.Load()
	s := r.reactive
	if s == nil {
		return nil, nil
	}
	if err := s.failure.Load(); err != nil {
		return nil, *err
	}
	if event.Stage == arb.QuoteInitial {
		return nil, nil
	}
	s.tail.observe(event.Result.Opportunities, max(event.Job.Version.BlockNumber, event.Job.Version.Anchor.Number))
	if !r.Current(event.Job.Version) {
		s.skip("version_superseded")
		return nil, nil
	}
	cache := s.cache.Load()
	switch {
	case event.Job.ReceivedAt.IsZero() || consumed.Before(event.Job.ReceivedAt):
		s.skip("missing_receive_time")
		return nil, nil
	case consumed.Sub(event.Job.ReceivedAt) >= s.settings.planning.Lifetime:
		s.skip("quote_older_than_lifetime")
		return nil, nil
	case event.Job.Version.Source == domain.SourceFast && r.canonicalLag > r.cfg.Network.Blocks(reactiveContextLag):
		s.skip("canonical_lag")
		return nil, nil
	case event.Job.Version.Source != domain.SourceFast && r.catchingUp && !r.nearTip:
		s.skip("catching_up")
		return nil, nil
	case event.Job.Version.Source == domain.SourceStandard && r.streamActive(time.Now()):
		s.skip("standard_while_fast_active")
		return nil, nil
	case cache == nil:
		s.skip("context_unavailable")
		return nil, nil
	case cache.costs.ExpiresMS <= consumed.UnixMilli():
		s.skip("context_expired")
		return nil, nil
	case cache.block.Number > event.Job.Version.Anchor.Number:
		s.skip("context_newer_than_anchor")
		return nil, nil
	case event.Job.Version.Anchor.Number-cache.block.Number > r.cfg.Network.Blocks(reactiveContextLag):
		// Block distance and context age separate a slow refresh from a
		// canonical jump that the refresh has not yet followed.
		built := time.UnixMilli(cache.costs.ExpiresMS - s.settings.document.ContextMaxAgeMS)
		s.skip(fmt.Sprintf("context_older_than_anchor blocks=%s age=%s", blockBucket(event.Job.Version.Anchor.Number-cache.block.Number), ageBucket(consumed.Sub(built))))
		return nil, nil
	}
	// With extra lanes a new intent takes a free lane instead of replacing
	// the primary's pending transaction.
	var pending *execution.PendingTransaction
	if s.dispatcher != nil && len(s.extra) == 0 {
		pending = s.dispatcher.Pending()
		if pending != nil && s.dispatcher.Included() {
			// Its nonce is consumed: the next intent takes the next nonce rather
			// than replacing a transaction that already landed.
			pending = nil
		}
		if pending != nil && pending.Cancellation != nil {
			s.skip("dispatch_lane_busy")
			return nil, nil
		}
	}
	if s.settings.document.LongTail {
		// event is this call's copy; the kept slice is new, so other
		// consumers of the result still see every opportunity.
		event.Result.Opportunities = s.tail.survivors(event.Result.Opportunities, event.Job.Version.Anchor.Number)
		if len(event.Result.Opportunities) == 0 {
			s.skip("long_tail_not_survived")
			return nil, nil
		}
	}
	offer := &reactiveOffer{event: event, engine: r.topology.Engine, cache: cache, pending: pending, revision: revision, generation: generation, lenders: r.flashLenders()}
	offer.timing.ReceivedNS, offer.timing.ConsumedNS = event.Job.ReceivedAt.UnixNano(), consumed.UnixNano()
	// A Fast version's anchor is its canonical base; its feed block is the
	// block it extends.
	feedAt, fed := r.feedArrivals.arrival(event.Job.Version.Anchor.Hash)
	if event.Job.Version.Source == domain.SourceFast {
		feedAt, fed = r.feedArrivals.arrivalAt(event.Job.Version.BlockNumber)
	}
	if fed {
		offer.timing.FeedNS = feedAt.UnixNano()
	}
	if s.bids != nil {
		if best := bestOpportunity(event.Result.Opportunities); best != nil {
			value, _, _ := r.research.NativeValue(event.Job.View, best.Cycle, s.settings.wrapped, best.Net)
			s.bids.register(best.Cycle.Pools, s.admissionWorth(value, len(best.Cycle.Pools)), consumed)
		}
	}
	if !event.QuotedAt.IsZero() {
		offer.timing.QuotedNS = event.QuotedAt.UnixNano()
	}
	if r.executionPaper != nil {
		verify := r.paperStateJob(event, consumed)
		offer.verify = &verify
	}
	s.metrics.Add(pipeline.MetricSource(event.Job.Version.Source), telemetry.ReactiveAdmitted, 1)
	r.silence.admittedAt = consumed
	return offer, nil
}

// buildReactive may run on the reactive worker: it reads only the captured
// offer, immutable settings, atomics and lock-protected state.
func (r *runtime) buildReactive(ctx context.Context, offer *reactiveOffer) {
	s, event, cache, pending := r.reactive, offer.event, offer.cache, offer.pending
	// A fast-lane build also counts its reasons apart: the full search admits
	// many routes the fast lane quoted earlier in the block.
	fast := offer.timing.Lane == "fast"
	skip := func(reason string) {
		s.skip(reason)
		if fast {
			s.skip("fast_build: " + reason)
		}
	}
	skipError := func(stage string, err error) { skip(skipReason(stage, err)) }
	// Every candidate, principal and bid price below quotes the same pools;
	// copying their tick state once per build took most of its time.
	view := arb.CachedView(event.Job.View)
	now := time.Now()
	offer.timing.BuildStartNS = now.UnixNano()
	if now.Sub(event.Job.ReceivedAt) >= s.settings.planning.Lifetime {
		skip("quote_older_than_lifetime")
		return
	}
	if cache.costs.ExpiresMS <= now.UnixMilli() {
		skip("context_expired")
		return
	}
	source := pipeline.MetricSource(event.Job.Version.Source)
	defer func() { s.metrics.Observe(source, telemetry.ReactiveBuild, time.Since(now)) }()
	steps := &buildSteps{}
	offer.timing.Build = steps
	ranking := time.Now()
	candidates := rankBySurvival(event.Result.Opportunities, r.survival, func(o arb.Opportunity) *big.Int {
		value, _, _ := r.research.NativeValue(view, o.Cycle, s.settings.wrapped, o.Net)
		return value
	})
	steps.Rank = time.Since(ranking).Nanoseconds()
	s.metrics.Observe(source, telemetry.ReactiveRank, time.Since(ranking))
	// Pools our own sends may have changed beyond this quote's view. A
	// replacement of the pending transaction runs instead of it, not after.
	moved := s.own.moved(event.Job.Version, pending, 0, now)
	held := s.own.held(now)
	var best *execution.ReactiveIntent
	var bestOpportunity arb.Opportunity
	var bestDependencies []domain.PoolKey
	var blocked *execution.ReactiveIntent
	var blockedValue float64
	var sealed []execution.ReactiveIntent // every admissible intent, for the extra lanes
	built, attempts := 0, 0
	for index := 0; index < len(candidates); index++ {
		quoted := candidates[index]
		cfg := s.settings.planning
		cfg.ReceivedAt, cfg.Lane = event.Job.ReceivedAt, offer.timing.Lane
		if pending != nil {
			cfg.MinimumFeeCap = execution.ReplacementFee(pending.Intent.Call.FeeCap)
			cfg.MinimumTip = execution.ReplacementFee(pending.Intent.Call.TipCap)
		}
		cfg.MinimumNet = s.settings.minimum[quoted.Cycle.Start]
		if cfg.MinimumNet == nil {
			skip("start_asset_not_configured")
			continue
		}
		if quoted.Input == nil || quoted.Net == nil || quoted.Net.Sign() <= 0 {
			skip("non_positive_quote")
			continue
		}
		// Any discovered pool the executor supports may be routed. Only the start
		// asset needs configuration; discovery decides which tokens are tradable
		// and the token risk registry excludes demonstrated transfer shortfalls.
		if reason := s.routeRejection(view, quoted.Cycle.Pools); reason != "" {
			skip(reason)
			continue
		}
		if overlaps(quoted.Cycle.Pools, moved) {
			skip("own_tx_not_in_quote")
			continue
		}
		if overlaps(quoted.Cycle.Pools, held) {
			skip("own_revert_unexplained")
			continue
		}
		// The cheapest this route can cost against what it is worth, with a
		// tenth of slack for the rounding between the two valuations.
		if gross, _, _ := r.research.NativeValue(view, quoted.Cycle, s.settings.wrapped, quoted.Net); gross != nil {
			floor := planning.NetworkCostFloor(quoted.Cycle.Pools, cache.costs, cfg)
			if new(big.Int).Mul(gross, big.NewInt(10)).Cmp(floor.Mul(floor, big.NewInt(9))) < 0 {
				skip("below_network_cost")
				continue
			}
		}
		if built == reactiveBuildCandidates || attempts == reactiveBuildAttempts {
			skip("below_build_rank")
			continue
		}
		built++
		attempts++
		// Native valuation comes from this exact pool view. Unknown conversion is
		// excluded; it is never treated as a zero native expense.
		unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
		valuing := time.Now()
		native, _, conversionKeys := r.research.NativeValue(view, quoted.Cycle, s.settings.wrapped, unit)
		steps.Native += time.Since(valuing).Nanoseconds()
		if native == nil || native.Sign() <= 0 {
			skip("native_value_unknown")
			continue
		}
		conversion := execution.FeeConversion{Numerator: unit, Denominator: native}
		// Probe smaller principals because a gross-optimal input can lose after
		// proportional loan fees. This is a bounded local refinement, not RPC work.
		// Only a V3 flash loan charges in proportion to the principal. With any
		// other funding at the gross-optimal input, a smaller principal has less
		// gross and the same costs, so it is not probed.
		fixedCost, kept := false, false
		for _, divisor := range []int64{1, 2, 4} {
			if fixedCost {
				break
			}
			o := quoted
			if divisor != 1 {
				input := new(big.Int).Quo(quoted.Input, big.NewInt(divisor))
				if input.Sign() == 0 {
					continue
				}
				var err error
				quoting := time.Now()
				o, err = offer.engine.QuoteCycle(ctx, view, quoted.Cycle, input)
				steps.Requote += time.Since(quoting).Nanoseconds()
				s.metrics.Observe(source, telemetry.ReactiveRequote, time.Since(quoting))
				if err != nil {
					continue
				}
			}
			if o.Input.Cmp(s.settings.limits.Assets[o.Cycle.Start]) > 0 {
				skip("principal_above_limit")
				continue
			}
			planned := time.Now()
			intent, err := planning.BuildReactive(ctx, view, o, offer.lenders, cache.wallet[o.Cycle.Start], event.Job.Version, conversion, cache.costs, cfg, now)
			steps.Plan += time.Since(planned).Nanoseconds()
			steps.Plans++
			s.metrics.Observe(source, telemetry.ReactivePlan, time.Since(planned))
			if err != nil {
				skipError("build", err)
				var short *planning.FundingShortfall
				if errors.As(err, &short) {
					skip("funding_cover_" + coverBucket(short.Gross, short.Budget))
				}
				// A smaller principal has less gross and the same budget.
				if errors.Is(err, planning.ErrNetworkUnprofitable) {
					break
				}
				continue
			}
			if len(intent.Call.Data) > s.settings.document.MaxCalldataBytes {
				skip("calldata_too_large")
				continue
			}
			if intent.Call.Value.Sign() > 0 && new(big.Int).Add(intent.Call.Value, intent.Budget).Cmp(cache.native) > 0 {
				skip("native_balance_short")
				continue
			}
			intent.Dependencies = append(append([]domain.PoolKey{}, o.Cycle.Pools...), conversionKeys...)
			if intent.Route.Funding == execution.FundingV3Flash {
				intent.Dependencies = append(intent.Dependencies, intent.Route.Lender)
			}
			intent.ExpiresMS = min(intent.ExpiresMS, event.Job.ReceivedAt.Add(cfg.Lifetime).UnixMilli())
			sealing := time.Now()
			intent, err = execution.SealReactiveIntent(ctx, intent)
			steps.Seal += time.Since(sealing).Nanoseconds()
			if err != nil {
				skipError("seal", err)
				continue
			}
			kept = true
			fixedCost = divisor == 1 && intent.Route.Funding != execution.FundingV3Flash
			// The same loop started on a pool that lends is the next
			// candidate; it shares this route's build slot.
			if divisor == 1 && intent.Route.Funding == execution.FundingV3Flash {
				configured := func(token domain.Address) bool { return s.settings.minimum[token] != nil }
				if cycle, input, ok := flashSwapTurn(view, quoted, configured); ok {
					if turned, err := offer.engine.QuoteCycle(ctx, view, cycle, input); err == nil {
						candidates = slices.Insert(candidates, index+1, turned)
						built--
					}
				}
			}
			if pending != nil && !execution.BetterReactive(intent, pending.Intent, s.settings.document.ReplacementGainBPS) {
				skip("not_better_than_pending")
				if value := expectedNative(intent, r.survival); blocked == nil || value > blockedValue {
					copy := intent
					blocked, blockedValue = &copy, value
				}
				continue
			}
			if len(s.extra) > 0 {
				sealed = append(sealed, intent)
			}
			if best == nil || expectedNative(intent, r.survival) > expectedNative(*best, r.survival) {
				copy := intent
				best = &copy
				bestOpportunity = o
				bestDependencies = intent.Dependencies
			}
		}
		// A candidate that sealed nothing gives its slot to the next one;
		// reactiveBuildAttempts still bounds the work.
		if !kept {
			built--
		}
		// Under arrival ordering the fast lane sends the first candidate that
		// builds. Candidates come in expected-value order and each further
		// build costs about 2 ms, more than the difference between neighbours
		// is worth under arrival ordering. Where
		// tips order a block, a few milliseconds do not move a transaction
		// inside its auction round, and the other candidates are the variants
		// the extra lanes send.
		if fast && best != nil && s.settings.planning.Ordering == domain.OrderingArrival {
			break
		}
	}
	if best == nil {
		if blocked != nil {
			s.recordPendingBlock(pending, *blocked, blockedValue, now)
		}
		skip("no_admissible_opportunity")
		return
	}
	current := r.reactiveGuard(bestDependencies, cache, offer.generation, offer.revision)
	if current == nil {
		skip("input_moved_during_build")
		return
	}
	offer.timing.SealedNS = time.Now().UnixNano()
	if offer.fastGap != nil {
		class := offer.fastGap[arb.RouteKey(bestOpportunity.Cycle)]
		if class == "" {
			class = "unranked"
		}
		skip("fast_gap_" + class)
		if origin := offer.routeOrigin[arb.RouteKey(bestOpportunity.Cycle)]; origin != "" {
			skip("intent_route_" + origin)
		}
		if strings.HasPrefix(class, "not_quoted") || class == "fast_not_run_dirty_not_quoted" {
			select {
			case s.diagnoses <- routeDiagnosis{view: event.Job.View, cycle: bestOpportunity.Cycle, dirty: event.Job.Dirty, class: class}:
			default:
			}
		}
	}
	job := reactiveJob{intent: *best, current: current, timing: offer.timing}
	if offer.verify != nil {
		verify := *offer.verify
		verify.opportunity, verify.intent = bestOpportunity, best
		job.verify = &verify
	}
	if pending != nil {
		job.target = pending.Hash
	}
	slices.SortStableFunc(sealed, func(a, b execution.ReactiveIntent) int {
		return cmp.Compare(expectedNative(b, r.survival), expectedNative(a, r.survival))
	})
	variants := exclusiveVariants(sealed, *best, len(s.extra))
	// Variants held through no common pool can still race if every one of
	// them claims the opportunity.
	if group := claimGroup(sealed, *best, len(s.extra), cache.costs.ClaimEpoch); len(group) > len(variants)+1 {
		if claimed, err := claimAll(ctx, group, cache.costs.ClaimEpoch); err != nil {
			skipError("claim", err)
		} else {
			job.intent, variants = claimed[0], claimed[1:]
			if job.verify != nil {
				job.verify.intent = &claimed[0]
			}
		}
	}
	if len(variants) > 0 {
		job.group = s.groups.Add(1)
	}
	s.metrics.Add(pipeline.MetricSource(event.Job.Version.Source), telemetry.ReactiveBuilt, 1)
	s.offerJob(job)
	r.offerDisjoint(sealed, job.intent, offer)
	r.offerVariants(variants, job.group, offer)
}

// claimGroup is best and up to n other conditional hot intents for its
// opportunity, most valuable first; nil when best is not one or the claim
// epoch is unread. Claiming at one epoch makes them pairwise exclusive: the
// first to succeed raises claimEpoch, which all the others hold.
func claimGroup(sealed []execution.ReactiveIntent, best execution.ReactiveIntent, n int, epoch uint64) []execution.ReactiveIntent {
	if epoch == 0 || !best.Route.Hot {
		return nil
	}
	group := []execution.ReactiveIntent{best}
	for _, intent := range sealed {
		if len(group) == n+1 {
			break
		}
		if intent.ID != best.ID && intent.Route.Hot && overlaps(routePools(intent), routePools(best)) {
			group = append(group, intent)
		}
	}
	return group
}

func claimAll(ctx context.Context, group []execution.ReactiveIntent, epoch uint64) ([]execution.ReactiveIntent, error) {
	claimed := make([]execution.ReactiveIntent, len(group))
	for i, intent := range group {
		var err error
		if claimed[i], err = execution.WithClaim(ctx, intent, epoch); err != nil {
			return nil, err
		}
	}
	return claimed, nil
}

// exclusiveVariants are up to n other intents for best's opportunity, most
// valuable first, that exclude best and each other pairwise: whichever of
// the group executes first voids all the others free. Exclusion with
// best alone is not enough: two variants held through different pools of
// best's route would both run once best reverts.
func exclusiveVariants(sealed []execution.ReactiveIntent, best execution.ReactiveIntent, n int) []execution.ReactiveIntent {
	group := []execution.ReactiveIntent{best}
	for _, intent := range sealed {
		if len(group) == n+1 {
			break
		}
		if intent.ID != best.ID && !slices.ContainsFunc(group, func(member execution.ReactiveIntent) bool { return !exclusive(member, intent) }) {
			group = append(group, intent)
		}
	}
	return group[1:]
}

// exclusive reports whether a and b hold a pool of both routes at the same
// slot words. The first of them to execute swaps through it, which changes
// those words and drops the other at its execution position; one that
// reverts leaves them and the other runs.
func exclusive(a, b execution.ReactiveIntent) bool {
	if a.Conditions == nil || b.Conditions == nil {
		return false
	}
	shared := map[domain.Address]bool{}
	for _, x := range routePools(a) {
		if slices.ContainsFunc(routePools(b), func(y domain.PoolKey) bool { return y == x }) {
			shared[x.Address] = true
		}
	}
	for _, x := range a.Conditions.Accounts {
		if !shared[x.Address] {
			continue
		}
		for _, y := range b.Conditions.Accounts {
			if y.Address != x.Address {
				continue
			}
			for _, slot := range x.Slots {
				if slices.Contains(y.Slots, slot) {
					return true
				}
			}
		}
	}
	return false
}

// offerVariants races best's variants on idle lanes, in best's group, so the
// queue and the in-flight checks let them run beside it.
func (r *runtime) offerVariants(variants []execution.ReactiveIntent, group uint64, offer *reactiveOffer) {
	s := r.reactive
	for _, intent := range variants {
		current := r.reactiveGuard(intent.Dependencies, offer.cache, offer.generation, offer.revision)
		if current == nil {
			s.skip("input_moved_during_build")
			return
		}
		s.metrics.Add(pipeline.MetricSource(intent.Version.Source), telemetry.ReactiveBuilt, 1)
		s.offerJob(reactiveJob{intent: intent, current: current, timing: offer.timing, group: group, variant: true})
	}
}

// offerDisjoint offers, for the extra lanes, the most valuable intents whose
// routes share no pool with best or with each other: one search can find
// opportunities that do not compete. sealed is most valuable first.
func (r *runtime) offerDisjoint(sealed []execution.ReactiveIntent, best execution.ReactiveIntent, offer *reactiveOffer) {
	s := r.reactive
	taken := routePools(best)
	offered := 0
	for _, intent := range sealed {
		if offered == len(s.extra) {
			return
		}
		pools := routePools(intent)
		if overlaps(pools, taken) {
			continue
		}
		current := r.reactiveGuard(intent.Dependencies, offer.cache, offer.generation, offer.revision)
		if current == nil {
			s.skip("input_moved_during_build")
			return
		}
		taken = append(taken, pools...)
		offered++
		s.metrics.Add(pipeline.MetricSource(intent.Version.Source), telemetry.ReactiveBuilt, 1)
		s.offerJob(reactiveJob{intent: intent, current: current, timing: offer.timing})
	}
}

func (s *reactiveRuntime) routeRejection(view arb.View, keys []domain.PoolKey) string {
	for _, key := range keys {
		if execution.CheckExecutorPool(key) != nil {
			return "pool_unsupported_by_executor"
		}
		if _, ok := view.Pool(key); !ok {
			return "pool_missing_from_view"
		}
	}
	return ""
}

// skip counts why an offer stopped. The writer reports and resets the totals.
func (s *reactiveRuntime) skip(reason string) {
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	if s.skips == nil {
		s.skips = map[string]uint64{}
	}
	// Error-derived keys are bounded so an unexpected message cannot grow the map.
	if _, known := s.skips[reason]; !known && len(s.skips) >= 64 {
		reason = "other"
	}
	s.skips[reason]++
}

var skipAddress = regexp.MustCompile(`0x[0-9a-fA-F]{40}`)

// skipError keeps the static prefix of an error; wrapped details carry amounts
// and addresses would give every pool its own counter.
func (s *reactiveRuntime) skipError(stage string, err error) { s.skip(skipReason(stage, err)) }

// skipReason folds an error into a bounded skip key.
func skipReason(stage string, err error) string {
	text, rest, _ := strings.Cut(skipAddress.ReplaceAllString(err.Error(), "<address>"), ":")
	// A one-word head is a package prefix ("pool: no liquidity"); the
	// reason is the next segment.
	if !strings.Contains(text, " ") && rest != "" {
		next, _, _ := strings.Cut(rest, ":")
		text += ":" + next
	}
	if len(text) > 60 {
		text = text[:60]
	}
	return stage + ": " + text
}

// coverBucket is gross as a share of the execution budget it fell short of.
func coverBucket(gross, budget *big.Int) string {
	if gross.Sign() <= 0 || budget.Sign() <= 0 {
		return "none"
	}
	percent := new(big.Int).Mul(gross, big.NewInt(100))
	percent.Quo(percent, budget)
	switch share := percent.Int64(); {
	case !percent.IsInt64() || share >= 100:
		return ">=100%"
	case share >= 75:
		return "75-100%"
	case share >= 50:
		return "50-75%"
	case share >= 25:
		return "25-50%"
	default:
		return "<25%"
	}
}

func blockBucket(blocks uint64) string {
	switch {
	case blocks <= 30:
		return "21-30"
	case blocks <= 60:
		return "31-60"
	default:
		return ">60"
	}
}

func ageBucket(age time.Duration) string {
	switch {
	case age < 2*time.Second:
		return "<2s"
	case age < 5*time.Second:
		return "2-5s"
	default:
		return ">=5s"
	}
}

func (s *reactiveRuntime) takeSkips() map[string]uint64 {
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	skips := s.skips
	s.skips = nil
	return skips
}

func nativeNet(i execution.ReactiveIntent) *big.Int {
	v := new(big.Int).Mul(i.ExpectedNet, i.Conversion.Denominator)
	return v.Quo(v, i.Conversion.Numerator)
}

// expectedNative weights an intent's native net by its route's survival.
func expectedNative(i execution.ReactiveIntent, survival *routeSurvival) float64 {
	if i.Bid != nil && i.Bid.Applied && i.Bid.CostVersion != "" && i.Bid.ModelTip.Cmp(i.Call.TipCap) == 0 {
		value, _ := new(big.Float).SetInt(i.Bid.Expected).Float64()
		return value
	}
	net, _ := new(big.Float).SetInt(nativeNet(i)).Float64()
	return net * survival.probability(len(i.Route.Steps))
}

// rankBySurvival orders candidates by native-valued gross weighted by how often
// routes of that length survive until our transaction lands; short routes are
// usually taken by faster rivals first. Gross is denominated in each
// route's start asset, so one event's USDC and OP routes are compared through
// native, which returns nil for a route it cannot value; those rank last.
func rankBySurvival(opportunities []arb.Opportunity, survival *routeSurvival, native func(arb.Opportunity) *big.Int) []arb.Opportunity {
	type scored struct {
		opportunity arb.Opportunity
		expected    float64
	}
	scores := make([]scored, len(opportunities))
	for i, o := range opportunities {
		scores[i] = scored{opportunity: o, expected: -1}
		if value := native(o); value != nil {
			gross, _ := new(big.Float).SetInt(value).Float64()
			scores[i].expected = gross * survival.probability(len(o.Cycle.Pools))
		}
	}
	slices.SortStableFunc(scores, func(a, b scored) int { return cmp.Compare(b.expected, a.expected) })
	ranked := make([]arb.Opportunity, len(scores))
	for i, s := range scores {
		ranked[i] = s.opportunity
	}
	return ranked
}
