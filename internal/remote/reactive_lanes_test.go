package remote

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

// laneJob is a job on pools worth net, enough for the queue's comparisons.
func laneJob(net int64, funding execution.Funding, pools ...byte) reactiveJob {
	route := execution.ExecutorPlan{Funding: funding}
	for _, p := range pools {
		route.Steps = append(route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{p}}})
	}
	intent := execution.ReactiveIntent{Route: route, ExpectedNet: big.NewInt(net), Conversion: execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, ExpiresMS: time.Now().Add(time.Minute).UnixMilli()}
	return reactiveJob{intent: intent, current: func() bool { return true }}
}

func twoLanes() *reactiveRuntime {
	s := &reactiveRuntime{jobs: make(chan struct{}, 1)}
	s.extra = []*sendLane{{index: 1, sender: domain.Address{0xe1}, notify: make(chan struct{}, 1)}}
	return s
}

func TestLaneSlotsKeepDisjointJobsAndReplaceOverlappingOnes(t *testing.T) {
	s := twoLanes()
	s.offerJob(laneJob(10, execution.FundingV3FlashSwap, 1, 2))
	s.offerJob(laneJob(20, execution.FundingV3FlashSwap, 3, 4))
	if len(s.slots) != 2 {
		t.Fatal("a disjoint job replaced another while a lane was free")
	}
	// Overlapping: a worse quote is dropped, a better one replaces.
	s.offerJob(laneJob(5, execution.FundingV3FlashSwap, 2, 9))
	if len(s.slots) != 2 || s.slots[0].intent.ExpectedNet.Int64() != 10 {
		t.Fatal("a worse overlapping quote replaced a job")
	}
	s.offerJob(laneJob(15, execution.FundingV3FlashSwap, 2, 9))
	if len(s.slots) != 2 || s.slots[1].intent.ExpectedNet.Int64() != 15 {
		t.Fatal("a better overlapping quote did not replace its job", len(s.slots))
	}
	// Every lane spoken for: a new disjoint job must beat the worst one.
	s.offerJob(laneJob(12, execution.FundingV3FlashSwap, 7))
	if len(s.slots) != 2 {
		t.Fatal("a third job queued with two lanes")
	}
	s.offerJob(laneJob(30, execution.FundingV3FlashSwap, 8))
	nets := map[int64]bool{}
	for _, q := range s.slots {
		nets[q.intent.ExpectedNet.Int64()] = true
	}
	if !nets[20] || !nets[30] {
		t.Fatal("the worst job was not the one replaced", nets)
	}
}

// A running job that a better overlapping quote replaces fails its final
// check; with one lane this is the earlier single slot.
func TestLaneSlotsSupersedeARunningJob(t *testing.T) {
	s := &reactiveRuntime{jobs: make(chan struct{}, 1)}
	s.offerJob(laneJob(10, execution.FundingWallet, 1))
	running := s.takeJob(s.primary())
	s.offerJob(laneJob(11, execution.FundingWallet, 2))
	if running.current() {
		t.Fatal("a better job left the running one current on a single lane")
	}
	if next := s.takeJob(s.primary()); next == nil || next.intent.ExpectedNet.Int64() != 11 {
		t.Fatal("the better job was not queued")
	}
}

func TestExtraLanesTakeOnlyFlashFundedJobsOnceFunded(t *testing.T) {
	s := twoLanes()
	lane := s.extra[0]
	s.offerJob(laneJob(10, execution.FundingWallet, 1))
	if s.takeJob(lane) != nil {
		t.Fatal("an extra lane took a wallet-funded job")
	}
	s.offerJob(laneJob(9, execution.FundingV3FlashSwap, 2))
	lane.dispatcher = &execution.Dispatcher{}
	if s.takeJob(lane) != nil {
		t.Fatal("an unfunded live lane took a job")
	}
	lane.funded.Store(true)
	if job := s.takeJob(lane); job == nil || job.intent.Route.Funding != execution.FundingV3FlashSwap {
		t.Fatal("a funded lane left its job")
	}
	if job := s.takeJob(s.primary()); job == nil || job.intent.Route.Funding != execution.FundingWallet {
		t.Fatal("the primary did not take the wallet-funded job")
	}
}

func TestOwnFlightRefusesARouteAnotherLaneIsSending(t *testing.T) {
	var f ownFlight
	a, b := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	if !f.reserve(0, []domain.PoolKey{a}, 0) || f.reserve(1, []domain.PoolKey{a, b}, 0) || !f.reserve(1, []domain.PoolKey{b}, 0) {
		t.Fatal("reservations did not follow the routes in flight")
	}
	f.release(0)
	if !f.reserve(2, []domain.PoolKey{a}, 0) {
		t.Fatal("a released route stayed reserved")
	}
}

// A flash-funded intent built for the primary goes out from the second lane,
// signed by that lane's key against its own nonce.
func TestSecondLaneSendsTheIntentAsItsOwnSender(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	start := event.Result.Opportunities[0].Cycle.Start
	lender := domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(30)}
	if err := r.store.Upsert(pool.State{Key: lender, Token0: start, Token1: addressAt(3), Fee: 500, TickSpacing: 10, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1_000_000)}); err != nil {
		t.Fatal(err)
	}
	event.Job.View = r.store.View()
	cache := *s.cache.Load()
	cache.wallet = map[domain.Address]*big.Int{}
	s.cache.Store(&cache)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := updateSigner{key}
	limits := s.settings.limits
	limits.Chain, limits.Executor, limits.CodeHash = s.settings.planning.Chain, s.settings.planning.Executor, s.settings.planning.CodeHash
	limits.Sender, limits.PerTransaction, limits.PerDay = signer.Address(), big.NewInt(1000000), big.NewInt(10000000)
	limits.StopFile = filepath.Join(t.TempDir(), "STOP")
	limits.TokenCheck = func(domain.Address) error { return nil }
	lane := &sendLane{index: 1, sender: signer.Address(), notify: make(chan struct{}, 1)}
	if lane.dispatcher, err = execution.OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch-2.json"), limits, signer, network); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lane.dispatcher.Close() })
	lane.funded.Store(true)
	s.extra = []*sendLane{lane}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(lane)
	if job == nil {
		t.Fatal("no flash-funded job for the second lane", s.takeSkips())
	}
	result := s.executeJob(t.Context(), job, lane)
	hash, ok := result["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) || result["error"] != nil || result["lane"] != 1 || network.calls != 1 {
		t.Fatal("second lane did not send", result["status"], result["error"])
	}
	pending := lane.dispatcher.Pending()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(pending.Raw); err != nil {
		t.Fatal(err)
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
	if err != nil || domain.Address(from) != signer.Address() || pending.Intent.Call.Sender != signer.Address() || tx.Nonce() != 0 {
		t.Fatal("the second lane's transaction is not its own", from, pending.Intent.Call.Sender)
	}
	if s.dispatcher.Pending() != nil {
		t.Fatal("the primary lane was used")
	}
}

// held is a condition holding pool p at word v.
func held(j reactiveJob, p byte, v byte) reactiveJob {
	c := &execution.TransactionConditions{TimestampMax: 1}
	c.Hold(domain.Address{0xee}, []execution.SlotCondition{{Value: domain.Hash{1}}}) // the executor's slot 0
	c.Hold(domain.Address{p}, []execution.SlotCondition{{Slot: domain.Hash{4}, Value: domain.Hash{v}}})
	j.intent.Conditions = c
	return j
}

func TestVariantsAreExclusiveOnlyThroughASharedHeldPool(t *testing.T) {
	a := held(laneJob(10, execution.FundingV3FlashSwap, 1, 2), 2, 7)
	b := held(laneJob(9, execution.FundingV3FlashSwap, 3, 2), 2, 7)
	if !exclusive(a.intent, b.intent) {
		t.Fatal("variants holding the same pool at the same word are not exclusive")
	}
	if exclusive(a.intent, held(laneJob(9, execution.FundingV3FlashSwap, 3, 2), 2, 8).intent) {
		t.Fatal("different words of the pool made them exclusive")
	}
	// Both hold the executor's slot 0, which a success does not move.
	if exclusive(a.intent, held(laneJob(9, execution.FundingV3FlashSwap, 2, 5), 5, 7).intent) {
		t.Fatal("a shared pool that neither holds made them exclusive")
	}
}

func TestVariantsTakeOnlyIdleLanesAndGoFirst(t *testing.T) {
	s := twoLanes()
	s.extra = append(s.extra, &sendLane{index: 2, notify: make(chan struct{}, 1)})
	best := laneJob(10, execution.FundingV3FlashSwap, 1, 2)
	best.group = 7
	s.offerJob(best)
	variant := laneJob(9, execution.FundingV3FlashSwap, 3, 2)
	variant.group, variant.variant = 7, true
	s.offerJob(variant)
	if len(s.slots) != 2 {
		t.Fatal("a variant of the same group did not run beside its best")
	}
	stranger := laneJob(20, execution.FundingV3FlashSwap, 3, 9)
	stranger.variant, stranger.group = true, 8
	s.offerJob(stranger)
	if len(s.slots) != 2 {
		t.Fatal("a variant replaced another group's job")
	}
	s.offerJob(laneJob(5, execution.FundingV3FlashSwap, 4))
	s.offerJob(laneJob(6, execution.FundingV3FlashSwap, 8))
	nets := map[int64]bool{}
	for _, q := range s.slots {
		nets[q.intent.ExpectedNet.Int64()] = true
	}
	if len(s.slots) != 3 || nets[9] || !nets[10] {
		t.Fatal("a full queue kept the variant over an opportunity", nets)
	}
}

func TestOwnFlightLetsAVariantGroupShareItsPools(t *testing.T) {
	var f ownFlight
	a := domain.PoolKey{Address: domain.Address{1}}
	if !f.reserve(0, []domain.PoolKey{a}, 3) || !f.reserve(1, []domain.PoolKey{a}, 3) || f.reserve(2, []domain.PoolKey{a}, 4) {
		t.Fatal("reservations ignored the variant group")
	}
	now := time.Now()
	f.sent(0, 5, []domain.PoolKey{a}, 3, now)
	version := domain.StateVersion{BlockNumber: 10}
	if len(f.moved(version, nil, 3, now)) != 0 || len(f.moved(version, nil, 0, now)) != 1 {
		t.Fatal("moved ignored the variant group")
	}
}

// Two variants held through different pools of best's route both run once
// best reverts; only pairwise exclusive variants join the group.
func TestVariantGroupsArePairwiseExclusive(t *testing.T) {
	best := heldTwice(laneJob(10, execution.FundingV3FlashSwap, 1, 2, 3), 2, 3)
	onTwo := held(laneJob(9, execution.FundingV3FlashSwap, 2, 4), 2, 7)
	onThree := held(laneJob(8, execution.FundingV3FlashSwap, 3, 5), 3, 7)
	best.intent.ID, onTwo.intent.ID, onThree.intent.ID = domain.Hash{1}, domain.Hash{2}, domain.Hash{3}
	variants := exclusiveVariants([]execution.ReactiveIntent{best.intent, onTwo.intent, onThree.intent}, best.intent, 2)
	if len(variants) != 1 || variants[0].ID != onTwo.intent.ID {
		t.Fatal("variants that do not exclude each other joined one group", len(variants))
	}
}

// heldTwice holds pools p and q at word 7.
func heldTwice(j reactiveJob, p, q byte) reactiveJob {
	j = held(j, p, 7)
	j.intent.Conditions.Hold(domain.Address{q}, []execution.SlotCondition{{Slot: domain.Hash{4}, Value: domain.Hash{7}}})
	return j
}

// Without a common held pool, an opportunity's hot intents race only by
// claiming it, and only when the claim epoch is known.
func TestClaimGroupsTakeOverlappingHotIntents(t *testing.T) {
	hot := func(net int64, id byte, pools ...byte) execution.ReactiveIntent {
		j := laneJob(net, execution.FundingV3FlashSwap, pools...)
		j.intent.Route.Hot, j.intent.ID = true, domain.Hash{id}
		return j.intent
	}
	best := hot(10, 1, 1, 2)
	sealed := []execution.ReactiveIntent{best, hot(9, 2, 2, 3), hot(8, 3, 4, 5), hot(7, 4, 1, 6)}
	packed := laneJob(6, execution.FundingV3FlashSwap, 1, 7).intent
	sealed = append(sealed, packed)
	group := claimGroup(sealed, best, 3, 5)
	if len(group) != 3 || group[0].ID != best.ID || group[1].ID != (domain.Hash{2}) || group[2].ID != (domain.Hash{4}) {
		t.Fatal("claim group", len(group))
	}
	if claimGroup(sealed, best, 3, 0) != nil || claimGroup(sealed, packed, 3, 5) != nil {
		t.Fatal("claimed without an epoch or from a packed route")
	}
}
