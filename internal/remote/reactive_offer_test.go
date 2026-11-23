package remote

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func reactiveFixture(t *testing.T) (*runtime, arb.SearchEvent) {
	t.Helper()
	store, pools := poolFixture(t)
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(100000) }})
	if err != nil {
		t.Fatal(err)
	}
	cycle := graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}
	quoted, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100000))
	if err != nil {
		t.Fatal(err)
	}
	cfg := planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 21000, MaxFee: big.NewInt(100), MaxTip: new(big.Int), TargetTip: new(big.Int), MaxTotalFee: big.NewInt(100000), MinimumNet: big.NewInt(1), SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	s := &reactiveRuntime{jobs: make(chan struct{}, 1), settings: reactiveSettings{planning: cfg, wrapped: cycle.Start, minimum: map[domain.Address]*big.Int{cycle.Start: big.NewInt(1)}, limits: execution.DispatchLimits{Assets: map[domain.Address]*big.Int{pools[0].Token0: big.NewInt(1000000), pools[0].Token1: big.NewInt(1000000), pools[1].Token0: big.NewInt(1000000), pools[1].Token1: big.NewInt(1000000)}}, document: reactivePolicy{MaxCalldataBytes: 4096}}}
	s.cache.Store(&reactiveCache{costs: planning.ReactiveCosts{BaseFee: big.NewInt(1), Extra: new(big.Int), ExpiresMS: time.Now().Add(time.Second).UnixMilli()}, wallet: map[domain.Address]*big.Int{cycle.Start: big.NewInt(1000000)}, native: big.NewInt(1000000), block: store.Block()})
	version := domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block(), PrefixHash: store.Block().Hash}
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyOPStack, ChainID: 10, BlockTimeMS: 2000}}, reactive: s, version: version, store: store, topology: topology}
	event := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{Version: version, View: store.View(), ReceivedAt: time.Now()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quoted}}}
	return r, event
}

func TestReactiveOfferUsesCachedContextAndInvalidatesOnFrame(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	var err error
	// No RPC client or signer is installed: offer must use the immutable cache.
	if err = r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 {
		t.Fatal("profitable cached opportunity not admitted")
	}
	<-s.jobs
	job := s.takeJob(s.primary())
	if !job.current() {
		t.Fatal("fresh admission invalidated")
	}
	r.reactiveRevision.Add(1)
	if job.current() {
		t.Fatal("canonical/frame transition retained stale admission")
	}
	event.Job.ReceivedAt = time.Now().Add(-time.Second)
	if err = r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 0 {
		t.Fatal("stale quote admitted")
	}
}

func TestReactiveOfferCountsSkipReasons(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	// Only the start asset is configured: an intermediate token needs no entry.
	other := r.store.Pools()[0].Token1
	if other == event.Result.Opportunities[0].Cycle.Start {
		other = r.store.Pools()[0].Token0
	}
	delete(s.settings.limits.Assets, other)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 {
		t.Fatal("route through an unlisted intermediate token was rejected", s.takeSkips())
	}
	<-s.jobs
	s.finishJob(s.takeJob(s.primary()))
	delete(s.settings.minimum, event.Result.Opportunities[0].Cycle.Start)
	event.Job.ReceivedAt = time.Now()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.Job.ReceivedAt = time.Now().Add(-time.Second)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	skips := s.takeSkips()
	if skips["start_asset_not_configured"] != 1 || skips["no_admissible_opportunity"] != 1 || skips["quote_older_than_lifetime"] != 1 {
		t.Fatal("skip reasons not counted", skips)
	}
	if len(s.takeSkips()) != 0 {
		t.Fatal("skip counters were not reset after reporting")
	}
}

func TestReactiveFastOfferToleratesBoundedCanonicalLag(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	anchor := r.store.Block()
	anchor.Number += r.cfg.Network.Blocks(reactiveContextLag)
	anchor.Hash = domain.Hash{0xfa}
	fast := domain.StateVersion{Source: domain.SourceFast, Anchor: anchor, BlockNumber: anchor.Number + 1, PrefixHash: domain.Hash{0xfb}}
	r.tentative = fast
	event.Job.Version = fast
	// OP canonical reads trail the Fast head by about two blocks in steady state.
	r.catchingUp, r.nearTip, r.canonicalLag = true, false, 2
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 {
		t.Fatal("fast opportunity rejected within the canonical lag bound", s.takeSkips())
	}
	<-s.jobs
	s.finishJob(s.takeJob(s.primary()))

	r.canonicalLag = r.cfg.Network.Blocks(reactiveContextLag) + 1
	event.Job.ReceivedAt = time.Now()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 0 || s.takeSkips()["canonical_lag"] != 1 {
		t.Fatal("fast opportunity admitted beyond the canonical lag bound")
	}

	standard := r.version
	event.Job.Version = standard
	event.Job.ReceivedAt = time.Now()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 0 || s.takeSkips()["catching_up"] != 1 {
		t.Fatal("standard opportunity admitted while canonical catches up")
	}
}

func TestReactiveOfferBucketsStaleContextByBlocksAndAge(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	s.settings.document.ContextMaxAgeMS = 5000
	anchor := r.store.Block()
	anchor.Number += 25
	anchor.Hash = domain.Hash{0xfa}
	fast := domain.StateVersion{Source: domain.SourceFast, Anchor: anchor, BlockNumber: anchor.Number + 1, PrefixHash: domain.Hash{0xfb}}
	r.tentative, event.Job.Version = fast, fast
	cache := *s.cache.Load()
	// The context was built a second ago and stays valid for four more.
	cache.costs.ExpiresMS = time.Now().Add(4 * time.Second).UnixMilli()
	s.cache.Store(&cache)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if got := s.takeSkips(); got["context_older_than_anchor blocks=21-30 age=<2s"] != 1 {
		t.Fatal("stale context was not bucketed", got)
	}
}

func TestReactiveBuildsOnlyTopGrossCandidates(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	base := event.Result.Opportunities[0]
	event.Result.Opportunities = nil
	for i := 0; i < 5; i++ {
		o := base
		o.Net = new(big.Int).Add(base.Net, big.NewInt(int64(i)))
		event.Result.Opportunities = append(event.Result.Opportunities, o)
	}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if skips := s.takeSkips(); skips["below_build_rank"] != 2 || len(s.jobs) != 1 {
		t.Fatal("lower-ranked candidates were repriced", skips, len(s.jobs))
	}
}

// below_build_rank can drop many candidates while the three builds a trigger
// allows produce nothing. A candidate that seals no intent gives its slot to
// the next one.
func TestReactiveBuildSlotReturnsWhenACandidateSealsNothing(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	base := event.Result.Opportunities[0]
	limit := s.settings.limits.Assets[base.Cycle.Start]
	event.Result.Opportunities = nil
	for i := 0; i < 5; i++ {
		o := base
		o.Net = new(big.Int).Add(base.Net, big.NewInt(int64(i)))
		if i >= 2 {
			// The three best candidates ask for more than the asset's cap.
			o.Input = new(big.Int).Add(limit, big.NewInt(1))
		}
		event.Result.Opportunities = append(event.Result.Opportunities, o)
	}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if skips := s.takeSkips(); skips["principal_above_limit"] != 3 || skips["below_build_rank"] != 0 || len(s.jobs) != 1 {
		t.Fatal("the candidates behind three failed builds were not built", skips, len(s.jobs))
	}
}

func TestReactiveWorkerBuildsLatestOfferWithTimeline(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	s.offerReady, s.buildDone = make(chan struct{}, 1), make(chan struct{})
	event.QuotedAt = time.Now()
	if err := r.queueReactive(event); err != nil {
		t.Fatal(err)
	}
	event.QuotedAt = time.Now()
	if err := r.queueReactive(event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go s.buildLoop(ctx, r)
	select {
	case <-s.jobs:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not build the queued offer")
	}
	cancel()
	<-s.buildDone
	job := s.takeJob(s.primary())
	if s.takeSkips()["offer_superseded"] != 1 {
		t.Fatal("older unbuilt offer was not superseded")
	}
	tm := job.timing
	if tm.ReceivedNS == 0 || tm.QuotedNS < tm.ReceivedNS || tm.ConsumedNS < tm.QuotedNS || tm.BuildStartNS < tm.ConsumedNS || tm.SealedNS < tm.BuildStartNS {
		t.Fatal("intent timeline missing or out of order", tm)
	}
}

func TestReactiveFundsWithStandaloneV3Lender(t *testing.T) {
	r, event := reactiveFixture(t)
	start := event.Result.Opportunities[0].Cycle.Start
	lender := domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(30)}
	if err := r.store.Upsert(pool.State{Key: lender, Token0: start, Token1: addressAt(3), Fee: 500, TickSpacing: 10, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1_000_000)}); err != nil {
		t.Fatal(err)
	}
	event.Job.View = r.store.View()
	cache := *r.reactive.cache.Load()
	cache.wallet = map[domain.Address]*big.Int{} // Nothing to fund the route from the wallet.
	r.reactive.cache.Store(&cache)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(r.reactive.jobs) != 1 {
		t.Fatal("route without wallet funds was not funded by the V3 lender", r.reactive.takeSkips())
	}
	<-r.reactive.jobs
	intent := r.reactive.takeJob(r.reactive.primary()).intent
	if intent.Route.Funding != execution.FundingV3Flash || intent.Route.Lender != lender || !slices.Contains(intent.Dependencies, lender) {
		t.Fatal("lender missing from route or send guard", intent.Route.Funding, intent.Dependencies)
	}
}

// A package prefix alone would name every such build skip "pool"; the key
// keeps the reason after it. Reasons that already read as text are unchanged.
func TestSkipKeysKeepTheReasonAfterAPackagePrefix(t *testing.T) {
	s := &reactiveRuntime{}
	s.skipError("build", fmt.Errorf("quote: %w", errors.New("x")))
	s.skipError("build", fmt.Errorf("%w: tick 5", pool.ErrNoLiquidity))
	s.skipError("build", errors.New("gross profit does not cover funding and execution budget: 7 < 9"))
	got := s.takeSkips()
	for _, key := range []string{"build: quote: x", "build: pool: no liquidity", "build: gross profit does not cover funding and execution budget"} {
		if got[key] != 1 {
			t.Fatalf("missing %q in %v", key, got)
		}
	}
}

// Wallet funding has no principal-proportional cost: once the gross-optimal
// input builds, smaller principals are not requoted or planned.
func TestReactiveBuildSkipsSmallerPrincipalsForFixedCostFunding(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	s.metrics = &telemetry.Registry{}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil || job.intent.Route.Funding != execution.FundingWallet {
		t.Fatal("expected a wallet-funded intent", job, s.takeSkips())
	}
	stages := s.metrics.Snapshot().Sources["standard"].Stages
	if stages["reactive_build"].Count != 1 {
		t.Fatal("one build not timed once", stages["reactive_build"].Count)
	}
	if stages["reactive_requote"].Count != 0 || stages["reactive_plan"].Count != 1 {
		t.Fatal("smaller principals were still probed", stages["reactive_requote"].Count, stages["reactive_plan"].Count)
	}
}

// The full search admits results tens of times a second. A fast-lane offer
// waiting for the build worker must not be replaced by them: it is built
// first, and the newest full-search offer is still built after it.
func TestFastLaneOfferIsBuiltBeforeLaterSearchOffers(t *testing.T) {
	s := &reactiveRuntime{offerReady: make(chan struct{}, 1)}
	fast := &reactiveOffer{timing: reactiveTiming{Lane: "fast"}}
	older := &reactiveOffer{}
	newer := &reactiveOffer{}
	s.queueOffer(fast)
	s.queueOffer(older)
	s.queueOffer(newer)
	if got := s.takeOffer(); got != fast {
		t.Fatal("fast-lane offer was not built first")
	}
	select {
	case <-s.offerReady:
	default:
		t.Fatal("pending search offer left without a wake-up")
	}
	if got := s.takeOffer(); got != newer {
		t.Fatal("newest search offer was not kept")
	}
	if got := s.takeOffer(); got != nil {
		t.Fatal("superseded search offer was built")
	}
	if skips := s.takeSkips(); skips["offer_superseded_worker"] != 1 || skips["offer_superseded_fast"] != 0 {
		t.Fatal("supersedes counted per lane wrongly", skips)
	}
}

// A fast-lane offer whose three highest-ranked candidates fail the network
// budget must still try lower-ranked ones. Longer routes rank higher at equal
// gross and carry a larger gas budget, so a shorter route below them can still
// pay.
func TestReactiveBudgetRejectionKeepsItsBuildSlot(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	cfg := &s.settings.planning
	cfg.GasLimit, cfg.RouteGasBase, cfg.RouteGasPerStep = 100000, 10000, 10000
	// Base fee 1 with the default 2x headroom: two pools cost 60,000, three 80,000.
	short := event.Result.Opportunities[0]
	short.Net = big.NewInt(65000)
	event.Result.Opportunities = []arb.Opportunity{short}
	for i := range 4 {
		extra := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: addressAt(byte(30 + i))}, Token0: addressAt(1), Token1: addressAt(2), Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(1_000_000), Fee: 3000}
		if err := r.store.Upsert(extra); err != nil {
			t.Fatal(err)
		}
		long := short
		long.Cycle.Pools = append(slices.Clone(short.Cycle.Pools), extra.Key)
		long.Cycle.Len = 3
		// Within a tenth of the 80,000 floor: only the builder rejects it.
		long.Net = big.NewInt(75000 + int64(i))
		event.Result.Opportunities = append(event.Result.Opportunities, long)
	}
	// Far below what two pools cost: dropped before it is counted.
	worthless := short
	worthless.Net = big.NewInt(10000)
	event.Result.Opportunities = append(event.Result.Opportunities, worthless)
	event.Job.View = r.store.View()
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	skips := s.takeSkips()
	if len(s.jobs) != 1 {
		t.Fatalf("the route that covers its budget was not built: %v", skips)
	}
	if job := s.takeJob(s.primary()); len(job.intent.Route.Steps) != 2 {
		t.Fatalf("built a route of %d steps", len(job.intent.Route.Steps))
	}
	if skips["below_network_cost"] != 1 || skips["build: gross profit does not cover funding and execution budget"] != 4 {
		t.Fatalf("skips %v", skips)
	}
}

// Under arrival ordering a fast-lane offer is sealed from its first candidate
// that builds; the full search still prices the top candidates and keeps the
// best. Where tips order a block the fast lane compares them too: a few
// milliseconds do not move a transaction inside its auction round, and the
// other candidates are the variants the extra lanes send.
func TestFastLaneBuildStopsAtFirstBuiltCandidate(t *testing.T) {
	for _, tc := range []struct {
		lane      string
		ordering  domain.Ordering
		wantPlans int
	}{{"fast", domain.OrderingArrival, 1}, {"", domain.OrderingArrival, 3}, {"fast", domain.OrderingPriority, 3}} {
		lane, wantPlans := tc.lane, tc.wantPlans
		r, event := reactiveFixture(t)
		s := r.reactive
		s.settings.planning.Ordering = tc.ordering
		base := event.Result.Opportunities[0]
		event.Result.Opportunities = nil
		for i := range 3 {
			o := base
			o.Net = new(big.Int).Sub(base.Net, big.NewInt(int64(i)))
			event.Result.Opportunities = append(event.Result.Opportunities, o)
		}
		offer, err := r.admitReactive(event)
		if err != nil || offer == nil {
			t.Fatal("offer not admitted", err)
		}
		offer.timing.Lane = lane
		r.buildReactive(t.Context(), offer)
		job := s.takeJob(s.primary())
		if job == nil {
			t.Fatalf("lane %q: no intent", lane)
		}
		if got := job.timing.Build.Plans; got != wantPlans {
			t.Fatalf("lane %q: %d plans, want %d", lane, got, wantPlans)
		}
		if lane == "fast" && tc.ordering == domain.OrderingArrival && job.intent.QuotedGross.Cmp(base.Net) != 0 {
			t.Fatalf("fast lane built gross %s, want the first candidate's %s", job.intent.QuotedGross, base.Net)
		}
	}
}
