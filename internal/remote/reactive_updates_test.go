package remote

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type updateSigner struct{ key *ecdsa.PrivateKey }

func (s updateSigner) Address() domain.Address {
	return domain.Address(crypto.PubkeyToAddress(s.key.PublicKey))
}
func (s updateSigner) Sign(_ context.Context, tx *types.Transaction) (*types.Transaction, error) {
	return types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), s.key)
}

type updateNetwork struct{ calls int }

func (n *updateNetwork) PendingNonce(context.Context, domain.Address) (uint64, error) { return 0, nil }
func (n *updateNetwork) ConfirmedNonce(context.Context, domain.Address) (uint64, error) {
	return 0, nil
}
func (n *updateNetwork) Receipt(context.Context, domain.Hash) (*rpc.TransactionReceipt, error) {
	return nil, nil
}
func (n *updateNetwork) Diagnose(context.Context, execution.ReactiveIntent, *execution.Settlement) string {
	return ""
}
func (n *updateNetwork) Send(_ context.Context, raw []byte, _ *execution.TransactionConditions) (domain.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return domain.Hash{}, err
	}
	n.calls++
	return domain.Hash(tx.Hash()), nil
}

// liveReactiveFixture wires a real dispatcher to a fake network, so a test can
// count exactly what would have left the process.
func liveReactiveFixture(t *testing.T) (*runtime, arb.SearchEvent, *updateNetwork) {
	t.Helper()
	r, event := reactiveFixture(t)
	s := r.reactive
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := updateSigner{key}
	cfg := &s.settings.planning
	cfg.Sender = signer.Address()
	cfg.GasLimit = 100000
	cfg.MaxTip = big.NewInt(10)
	cfg.MaxTotalFee = big.NewInt(1000000)
	s.settings.document.Mode = "live"
	s.settings.document.CancelFailureGas = 100000
	s.settings.document.CancelGasLimit = 21000
	for _, p := range r.store.Pools() {
		p.Reserve0.Mul(p.Reserve0, big.NewInt(1000))
		p.Reserve1.Mul(p.Reserve1, big.NewInt(1000))
		if err = r.store.Upsert(p); err != nil {
			t.Fatal(err)
		}
		s.settings.limits.Assets[p.Token0] = big.NewInt(10000000)
		s.settings.limits.Assets[p.Token1] = big.NewInt(10000000)
	}
	cache := s.cache.Load()
	cache.wallet[event.Result.Opportunities[0].Cycle.Start] = big.NewInt(10000000)
	event.Job.View = r.store.View()
	event.Result.Opportunities[0], err = r.topology.Engine.QuoteCycle(t.Context(), event.Job.View, event.Result.Opportunities[0].Cycle, big.NewInt(1000000))
	if err != nil {
		t.Fatal(err)
	}
	limits := s.settings.limits
	limits.Chain, limits.Sender, limits.Executor, limits.CodeHash = cfg.Chain, cfg.Sender, cfg.Executor, cfg.CodeHash
	limits.PerTransaction, limits.PerDay = big.NewInt(1000000), big.NewInt(10000000)
	limits.StopFile = filepath.Join(t.TempDir(), "STOP")
	limits.TokenCheck = func(domain.Address) error { return nil }
	network := &updateNetwork{}
	s.dispatcher, err = execution.OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), limits, signer, network)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.dispatcher.Close() })
	return r, event, network
}

func TestRuntimeCancelsConsumedPendingRouteUsingCachedState(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	var err error
	if err = r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 {
		t.Fatal("initial intent missing")
	}
	<-s.jobs
	job := s.takeJob(s.primary())
	result := s.executeJob(t.Context(), job, s.primary())
	if result["error"] != nil {
		t.Fatal(result)
	}
	s.finishJob(job)
	p := s.dispatcher.Pending()
	if p == nil {
		t.Fatal("dispatch did not publish pending state")
	}
	changed := event.Result.Opportunities[0].Cycle.Pools[0]
	pool, _ := r.store.Pool(changed)
	pool.Reserve1 = new(big.Int).Set(pool.Reserve0)
	if err = r.store.Upsert(pool); err != nil {
		t.Fatal(err)
	}
	dirty := []domain.PoolKey{changed}
	s.touchPools(dirty)
	r.considerReactiveCancellation(t.Context(), r.store.View(), dirty, event.Job.Version)
	if len(s.jobs) != 1 {
		t.Fatal("consumed opportunity did not queue cancellation")
	}
	<-s.jobs
	cancellation := s.takeJob(s.primary())
	result = s.executeJob(t.Context(), cancellation, s.primary())
	if result["error"] != nil || result["action"] != "cancel" {
		t.Fatal(result)
	}
	if network.calls != 2 || s.dispatcher.Pending().Cancellation == nil {
		t.Fatal("cancellation not submitted")
	}
	if s.dispatcher.Pending().Previous[0].Hash != p.Hash {
		t.Fatal("original transaction lost")
	}
}

func TestReactiveQueueCoalescesAndInvalidatesInFlightSelection(t *testing.T) {
	r, event := reactiveFixture(t)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	s := r.reactive
	<-s.jobs
	first := s.takeJob(s.primary())
	next := first.intent
	next.ExpectedNet = new(big.Int).Add(next.ExpectedNet, big.NewInt(1))
	s.offerJob(reactiveJob{intent: next, current: func() bool { return true }})
	if first.current() {
		t.Fatal("older selection survived better quote")
	}
	s.offerJob(reactiveJob{intent: first.intent, current: func() bool { return true }})
	<-s.jobs
	best := s.takeJob(s.primary())
	if best.intent.ExpectedNet.Cmp(next.ExpectedNet) != 0 || !best.current() {
		t.Fatal("queue retained worse quote")
	}
	s.finishJob(first)
	if len(s.slots) != 1 || s.slots[0] != best {
		t.Fatal("older sender cleared newer selection")
	}
}

func TestReactiveGuardTracksRouteConversionAndLenderWithoutRejectingUnrelatedFrames(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	keys := append([]domain.PoolKey{}, event.Result.Opportunities[0].Cycle.Pools...)
	conversion := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{55}}
	keys = append(keys, conversion)
	guard := r.reactiveGuard(keys, s.cache.Load(), r.reactiveRevision.Load(), r.inputRevision.Load())
	r.inputRevision.Add(1)
	if guard() {
		t.Fatal("unprocessed frame allowed send")
	}
	s.touchPools([]domain.PoolKey{{Address: domain.Address{77}}})
	s.applied.Store(r.inputRevision.Load())
	if !guard() {
		t.Fatal("unrelated complete frame invalidated route")
	}
	s.touchPools([]domain.PoolKey{conversion})
	if guard() {
		t.Fatal("outside-route conversion change ignored")
	}
	guard = r.reactiveGuard(keys, s.cache.Load(), r.reactiveRevision.Load(), r.inputRevision.Load())
	s.touchPools(keys[:1])
	if guard() {
		t.Fatal("route change ignored")
	}
}

// The build worker seals a quote taken at one input revision; a frame applied
// before the guard snapshots pool counters must not become its baseline.
func TestReactiveGuardRefusesFrameAppliedDuringBuild(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	keys := event.Result.Opportunities[0].Cycle.Pools
	quoted := r.inputRevision.Load()
	r.inputRevision.Add(1)
	s.touchPools(keys)
	s.applied.Store(r.inputRevision.Load())
	if r.reactiveGuard(keys, s.cache.Load(), r.reactiveRevision.Load(), quoted) != nil {
		t.Fatal("guard adopted pool versions from a frame newer than the quote")
	}
}

func TestRemainingProfitRequoteRecognizesConsumptionAndUnknownState(t *testing.T) {
	r, event := reactiveFixture(t)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	s := r.reactive
	<-s.jobs
	job := s.takeJob(s.primary())
	if lost, _ := r.remainingReactiveLoss(t.Context(), r.store.View(), job.intent, s.cache.Load()); lost {
		t.Fatal("profitable route considered lost")
	}
	tight := job.intent
	tight.Route.Steps = append([]execution.ExecutorStep{}, tight.Route.Steps...)
	tight.Route.Steps[0].MinAmountOut = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	if lost, _ := r.remainingReactiveLoss(t.Context(), r.store.View(), tight, s.cache.Load()); !lost {
		t.Fatal("unexecutable leg minimum retained")
	}
	key := event.Result.Opportunities[0].Cycle.Pools[0]
	p, _ := r.store.Pool(key)
	p.Reserve1 = new(big.Int).Set(p.Reserve0)
	if err := r.store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	if lost, _ := r.remainingReactiveLoss(t.Context(), r.store.View(), job.intent, s.cache.Load()); !lost {
		t.Fatal("consumed opportunity retained")
	}
	missing := job.intent
	missing.Route.Steps = append([]execution.ExecutorStep{}, missing.Route.Steps...)
	missing.Route.Steps[0].Pool.Address = domain.Address{99}
	if lost, _ := r.remainingReactiveLoss(t.Context(), r.store.View(), missing, s.cache.Load()); lost {
		t.Fatal("missing pool treated as proven loss")
	}
}

func TestPausedGateStopsLiveSendsButNotCancellation(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	var err error
	s.gate, err = control.OpenGate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.metrics = &telemetry.Registry{}
	take := func() *reactiveJob {
		t.Helper()
		if len(s.jobs) != 1 {
			t.Fatal("job not queued")
		}
		<-s.jobs
		return s.takeJob(s.primary())
	}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	queued := take()
	// The pause lands after the job was admitted, as it would in an incident.
	if _, err := s.gate.Set(true, "operator", "incident", time.Now()); err != nil {
		t.Fatal(err)
	}
	result := s.executeJob(t.Context(), queued, s.primary())
	if result["status"] != "held" || network.calls != 0 || s.dispatcher.Pending() != nil {
		t.Fatalf("paused gate let a transaction out: %v calls=%d", result, network.calls)
	}
	if held := s.metrics.Snapshot().Sources["standard"].Counters["plans_held"] + s.metrics.Snapshot().Sources["fast"].Counters["plans_held"]; held != 1 {
		t.Fatalf("plans_held = %d", held)
	}
	s.finishJob(queued)

	if _, err := s.gate.Set(false, "operator", "clear", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := take()
	if result := s.executeJob(t.Context(), job, s.primary()); result["error"] != nil || network.calls != 1 {
		t.Fatalf("open gate blocked the send: %v", result)
	}
	s.finishJob(job)
	pending := s.dispatcher.Pending()
	if pending == nil {
		t.Fatal("no pending transaction")
	}

	if _, err := s.gate.Set(true, "operator", "incident", time.Now()); err != nil {
		t.Fatal(err)
	}
	changed := event.Result.Opportunities[0].Cycle.Pools[0]
	pool, _ := r.store.Pool(changed)
	pool.Reserve1 = new(big.Int).Set(pool.Reserve0)
	if err := r.store.Upsert(pool); err != nil {
		t.Fatal(err)
	}
	dirty := []domain.PoolKey{changed}
	s.touchPools(dirty)
	r.considerReactiveCancellation(t.Context(), r.store.View(), dirty, event.Job.Version)
	cancellation := take()
	result = s.executeJob(t.Context(), cancellation, s.primary())
	if result["error"] != nil || result["action"] != "cancel" || network.calls != 2 {
		t.Fatalf("pause blocked a cancellation: %v calls=%d", result, network.calls)
	}
}

func TestLiveSendIsReplayedAtItsQuoteStateButHeldSendIsNot(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	r.executionPaper = &executionPaper{jobs: make(chan paperExecutionJob, 1)}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	<-s.jobs
	job := s.takeJob(s.primary())
	if result := s.executeJob(t.Context(), job, s.primary()); result["error"] != nil || network.calls != 1 {
		t.Fatal("live send failed", result)
	}
	verify := s.takeVerification()
	if verify == nil || verify.intent == nil || verify.intent.ID != job.intent.ID {
		t.Fatal("sent intent was not handed to the quote-state replay")
	}
	s.finishJob(job)

	var err error
	if s.gate, err = control.OpenGate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.metrics = &telemetry.Registry{}
	if _, err := s.gate.Set(true, "operator", "incident", time.Now()); err != nil {
		t.Fatal(err)
	}
	held := *job
	if result := s.executeJob(t.Context(), &held, s.primary()); result["status"] != "held" || s.verificationPending() {
		t.Fatal("an intent that never left was queued for replay", result["status"])
	}
}

func TestIncludedSendFreesTheLaneForTheNextOpportunity(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	send := func() map[string]any {
		t.Helper()
		event.Job.ReceivedAt = time.Now()
		if err := r.offerReactive(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if len(s.jobs) != 1 {
			t.Fatal("opportunity was not admitted", s.takeSkips())
		}
		<-s.jobs
		job := s.takeJob(s.primary())
		defer s.finishJob(job)
		return s.executeJob(t.Context(), job, s.primary())
	}
	first := send()
	if first["error"] != nil || first["action"] != "submit" {
		t.Fatal(first)
	}
	nonce := s.dispatcher.Pending().Nonce
	s.dispatcher.ObserveIncluded([]domain.Hash{s.dispatcher.Pending().Hash})
	// The next quote's view already holds the first send's effects; a view
	// without them would skip its pools (own_flight.go).
	s.own.seen(0, nonce, event.Job.Version.BlockNumber, 0)
	second := send()
	if second["error"] != nil || second["action"] != "submit" || network.calls != 2 || s.dispatcher.Pending().Nonce != nonce+1 {
		t.Fatal("an included send kept the lane busy or was replaced", second["action"], second["error"], network.calls)
	}
}
