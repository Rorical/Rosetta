package remote

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

// reactivePreflight runs an intent against the deployed executor at its quote
// block before it is admitted or sent. The local quote cannot see a token that
// refuses transfers to the executor, so an intent with a positive quote can
// still revert in a transfer.
// A revert is traced off the send path, and a refused or taxed transfer is
// recorded in the token registry so search stops building the route. A route
// that passed skips the call until one of our transactions on it reverts
// (passed): a trap refuses every buy, so one pass clears its tokens, and the
// call typically costs 8-22 ms on the send path (p50-p90). Stale
// quotes on a passed route are no longer caught. A token whose owner enables a
// restriction later reverts one send; settlement records the token and forget
// drops the route's pass. A route whose funding source and every leg passed in
// other routes counts as passed too (legs, routeLegs), until it reverts.
type reactivePreflight struct {
	read  *rpc.Client
	trace *rpc.Client
	risks *tokenrisk.Registry
	// pools receives quarantine evidence from calls at the quote block, the
	// Nitro counterpart of OP's execution paper; nil disables it.
	pools    *poolrisk.Registry
	outcomes poolrisk.Tracker
	// protected tokens are never recorded: the start assets and wrapped
	// native. Losing one would stop every route, not just a trap's.
	protected map[domain.Address]bool
	// traceWait bounds how long a diagnosis waits for the trace endpoint to
	// reach the quote block; an archive provider lags the local node.
	traceWait time.Duration
	reverts   chan execution.ReactiveIntent
	// probes are Fast intents on unproven routes, checked off the send path
	// (probeLoop); probed throttles them per route.
	probes chan execution.ReactiveIntent
	// measures are passed routes to trace for the gas they use, which gas
	// records in the route history (measureGas); measured throttles them.
	measures chan gasMeasure
	gas      func(route []domain.PoolKey, funding execution.Funding, used uint64)
	done     chan struct{}
	mu       sync.Mutex
	traced   map[string]time.Time
	probed   map[string]time.Time
	measured map[string]time.Time
	passed   map[string]time.Time // route key -> when it passed
	// legs counts the passed routes that executed each funding source and leg.
	legs map[string]int
	// whole counts the passed routes by funding, start asset and pools
	// (wholeKey): what ran tells apart from a route proven leg by leg.
	whole map[string]int
	// refuted holds routes that reverted for us, and refutedLegs their legs:
	// neither proves a route by composition until a route with that leg passes
	// directly again; a refuted route needs its own pass. Both are saved.
	refuted, refutedLegs map[string]time.Time
	// store persists passed across restarts; empty keeps them in memory.
	store string
	dirty bool
	// executor is the configured executor. Saved passes and refutations of
	// another address are read as its own (rekey): they say what a route's
	// pools and tokens do to our legs, and a redeployed executor runs the
	// same legs.
	executor domain.Address
	// unprovenCover lets a Fast intent on a route that never ran be sent when
	// its expected net, in native, is at least this share (basis points) of
	// its fee budget; zero keeps such intents back (covers).
	unprovenCover uint64
	// lost holds what each of our reverted transactions refuted until its
	// diagnosis arrives (settled, diagnosed).
	lost map[domain.Hash]forgotten
}

var errPreflightRevert = errors.New("preflight reverted")

// errPreflightUnproven drops a Fast intent whose route never passed, in whole
// or leg by leg (routeLegs): a Fast
// view has no written block to call at, so its route is proven in the
// background instead (probe) and sends from the next opportunity on it.
var errPreflightUnproven = errors.New("route not proven for a Fast quote")

// The executor's InsufficientProfit() and Slippage() selectors: a probe that
// ends in either is inconclusive, never a pass.
const (
	insufficientProfit = "0x4e47f8ea"
	slippage           = "0x7dd37f70"
)

const (
	preflightTimeout       = 100 * time.Millisecond
	preflightRetraceAfter  = time.Minute
	preflightTracedEntries = 1024
	preflightPassEntries   = 4096
	preflightSaveEvery     = time.Minute
	// A route's gas is measured at most once per measureRouteAfter; diagnosis
	// traces and Fast route probes run at most once a minute per route. There
	// is no cap across routes: a burst of new routes needs them all.
	measureRouteAfter = 10 * time.Minute
)

func newReactivePreflight(ctx context.Context, read, trace *rpc.Client, risks *tokenrisk.Registry, pools *poolrisk.Registry, protected map[domain.Address]bool, store string, executor domain.Address, gas func([]domain.PoolKey, execution.Funding, uint64)) *reactivePreflight {
	p := &reactivePreflight{executor: executor, read: read, trace: trace, risks: risks, pools: pools, protected: protected, traceWait: 10 * time.Second, reverts: make(chan execution.ReactiveIntent, 8), probes: make(chan execution.ReactiveIntent, 32), measures: make(chan gasMeasure, 32), gas: gas, done: make(chan struct{}), traced: map[string]time.Time{}, probed: map[string]time.Time{}, store: store}
	p.load(time.Now())
	var loops sync.WaitGroup
	loops.Go(func() { p.diagnoseLoop(ctx) })
	loops.Go(func() { p.probeLoop(ctx) })
	go func() { loops.Wait(); close(p.done) }()
	return p
}

// check returns errPreflightRevert when the executor reverts at the quote
// block, and another error when the call could not be run. Both drop the
// intent: an unsimulated intent is never sent unless its route passed before,
// whole or leg by leg, which check reports as skipped.
func (p *reactivePreflight) check(ctx context.Context, intent execution.ReactiveIntent) (skipped bool, err error) {
	key := routeKey(intent)
	if p.proven(key) {
		switch intent.Version.Source {
		case domain.SourceStandard:
			p.queueMeasure(intent, intent.Call, intent.Version.Anchor.Hash)
		case domain.SourceFast:
			// Its block is not written yet; measureGas waits for it.
			p.queueMeasure(intent, intent.Call, domain.Hash{})
		}
		return true, nil
	}
	if intent.Version.Source == domain.SourceFast {
		select {
		case p.probes <- intent:
		default:
		}
		return false, errPreflightUnproven
	}
	if intent.Version.Source != domain.SourceStandard {
		return false, fmt.Errorf("preflight requires a canonical quote block, have source %v", intent.Version.Source)
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	var output string
	err = p.read.Call(ctx, "eth_call", []any{preflightCall(intent.Call), map[string]any{"blockHash": intent.Version.Anchor.Hash.Hex()}}, &output)
	if err == nil {
		p.pass(key, time.Now())
		p.observePools(intent, key, false)
		p.queueMeasure(intent, intent.Call, intent.Version.Anchor.Hash)
		return false, nil
	}
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) || (rpcErr.Code != 3 && !strings.Contains(rpcErr.Message, "execution reverted")) {
		return false, fmt.Errorf("preflight unavailable: %w", err)
	}
	if p.trace != nil {
		select {
		case p.reverts <- intent:
		default:
		}
	}
	return false, fmt.Errorf("%w: %s", errPreflightRevert, rpcErr.Error())
}

// observePools feeds one call at the quote block to the pool quarantine. The
// executor ran on the state the quote read, so a revert of the positive quote
// implicates the route's swap pools that never passed (poolrisk.Tracker). The
// flash lender is left out, as on the paper path: one lender funds many
// routes, and losing it to a bad route would stop all of them. A revert on a
// route holding an excluded token is the token's.
func (p *reactivePreflight) observePools(intent execution.ReactiveIntent, key string, reverted bool) {
	if p.pools == nil {
		return
	}
	steps := intent.Route.Steps
	keys := make([]domain.PoolKey, 0, len(steps))
	for _, step := range steps {
		if reverted && (p.risks.Check(step.TokenIn) != nil || p.risks.Check(step.TokenOut) != nil) {
			return
		}
		keys = append(keys, step.Pool)
	}
	for _, evidence := range p.outcomes.Observe(digest([]byte(key)), keys, reverted, intent.Version.Anchor) {
		evidence.Source = "reactive_preflight"
		if err := p.pools.Record(evidence); err != nil {
			slog.Warn("pool quarantine not recorded", "pool", evidence.Pool.Address.Hex(), "error", err)
			continue
		}
		slog.Warn("pool quarantined after preflight reverts", "pool", evidence.Pool.Address.Hex(), "reverts", evidence.Reverts, "routes", evidence.Routes)
	}
}

func preflightCall(call execution.ExecutorCall) map[string]any {
	return map[string]any{"from": call.Sender.Hex(), "to": call.Executor.Hex(), "data": "0x" + hex.EncodeToString(call.Data), "gas": fmt.Sprintf("0x%x", call.GasLimit), "maxFeePerGas": "0x" + call.FeeCap.Text(16), "maxPriorityFeePerGas": "0x" + call.TipCap.Text(16), "value": "0x" + call.Value.Text(16)}
}

func (p *reactivePreflight) diagnoseLoop(ctx context.Context) {
	ticker := time.NewTicker(preflightSaveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.save()
			return
		case <-ticker.C:
			p.save()
		case intent := <-p.reverts:
			if p.claim(intent, time.Now()) {
				p.diagnoseRevert(ctx, intent)
			}
		}
	}
}

// diagnoseRevert traces a preflight revert. Only a revert at the quote block
// that the trace did not pin on a token counts against the route's pools: blaming them before the
// diagnosis would let a trap token paired with a healthy pool quarantine it.
// A revert the trace could not explain says nothing about the pools.
func (p *reactivePreflight) diagnoseRevert(ctx context.Context, intent execution.ReactiveIntent) {
	recorded, frame, err := p.diagnose(ctx, intent)
	if err != nil {
		slog.Warn("preflight revert not diagnosed", "intent", intent.ID.Hex(), "error", err)
		return
	}
	for _, e := range recorded {
		slog.Warn("token refuses executor transfers; excluded", "token", e.Token.Hex(), "block", e.Block.Number)
	}
	if lenderTransferFailed(frame, intent.Route) {
		slog.Warn("preflight reverted in the flash lender's transfer; route pools not blamed", "lender", intent.Route.Lender.Address.Hex(), "asset", intent.Route.Asset.Hex(), "amount", intent.Route.Amount)
		return
	}
	// A Fast route's probe runs at the written block, after whoever took
	// the opportunity: only a call at the quote block tests the quote.
	// Blaming pools for such probe reverts quarantines busy, healthy pools.
	if intent.Version.Source != domain.SourceStandard {
		return
	}
	p.observePools(intent, routeKey(intent), true)
}

// lenderTransferFailed reports a V3 flash whose lender could not send the
// loan: a failed call to the lender holding a failed transfer of the asset.
// The route's swap pools did not cause it, and blaming them would quarantine
// healthy pools.
func lenderTransferFailed(frame tokenrisk.CallFrame, plan execution.ExecutorPlan) bool {
	if plan.Funding != execution.FundingV3Flash || plan.Lender == (domain.PoolKey{}) {
		return false
	}
	lender, asset := strings.ToLower(plan.Lender.Address.Hex()), strings.ToLower(plan.Asset.Hex())
	var walk func(tokenrisk.CallFrame) bool
	walk = func(f tokenrisk.CallFrame) bool {
		if strings.EqualFold(f.To, lender) && f.Error != "" {
			for _, call := range f.Calls {
				if strings.EqualFold(call.To, asset) && call.Error != "" && strings.HasPrefix(strings.ToLower(call.Input), "0xa9059cbb") {
					return true
				}
			}
		}
		return slices.ContainsFunc(f.Calls, walk)
	}
	return walk(frame)
}

// claim traces one route at most once a minute. A revert the trace cannot
// attribute to a token (a stale quote, a drained lender) repeats on every
// search of that route and would otherwise keep the archive endpoint busy.
func (p *reactivePreflight) claim(intent execution.ReactiveIntent, now time.Time) bool {
	key := routeKey(intent)
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.traced[key]; ok && now.Sub(last) < preflightRetraceAfter {
		return false
	}
	if len(p.traced) >= preflightTracedEntries {
		clear(p.traced)
	}
	p.traced[key] = now
	return true
}

// routeKey names what a preflight proves: the executor, the funding source
// and each leg's pool and direction. A trap refuses buys only, so direction
// matters, and it may allow one recipient but not another; the amounts do not.
// The key is persisted, so it is spelled out rather than formatted structs.
func routeKey(intent execution.ReactiveIntent) string {
	plan := intent.Route
	var key strings.Builder
	pool := func(k domain.PoolKey) {
		fmt.Fprintf(&key, "|%s:%d:%s:%s", k.Address.Hex(), k.Kind, k.PoolID.Hex(), k.Hooks.Hex())
	}
	fmt.Fprintf(&key, "%s|%d|%s", intent.Call.Executor.Hex(), plan.Funding, plan.Asset.Hex())
	pool(plan.Lender)
	for _, step := range plan.Steps {
		pool(step.Pool)
		fmt.Fprintf(&key, ">%s>%s", step.TokenIn.Hex(), step.TokenOut.Hex())
	}
	return key.String()
}

func (p *reactivePreflight) proven(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.passed[key]; ok {
		return true
	}
	legs := routeLegs(key)
	if _, ok := p.refuted[key]; ok || len(legs) == 0 {
		return false
	}
	for _, leg := range legs {
		if _, bad := p.refutedLegs[leg]; bad || p.legs[leg] == 0 && !metricLeg(leg) {
			return false
		}
	}
	return true
}

// metricLeg reports a leg through a MetricOmm pool. Such a leg counts as
// passed without a pass of its own: the pools trade WETH, WBTC and USDC, so
// no token on the leg can trap the executor, and a probe cannot pass a route
// whose profit ends with the next oracle update (about two seconds). A leg
// that reverted is still refuted.
func metricLeg(leg string) bool {
	parts := strings.SplitN(leg, "|", 4)
	if len(parts) != 4 || parts[0] != "leg" {
		return false
	}
	fields := strings.SplitN(parts[3], ":", 3)
	return len(fields) == 3 && fields[1] == strconv.Itoa(int(domain.PoolKindMetricOmm))
}

// rekey reads a saved route key, or a leg key, as the configured executor's.
func (p *reactivePreflight) rekey(key string) string {
	if p.executor == (domain.Address{}) {
		return key
	}
	parts := strings.Split(key, "|")
	at := 0
	if parts[0] == "leg" || parts[0] == "fund" {
		at = 1
	}
	if at >= len(parts) || !strings.HasPrefix(parts[at], "0x") || len(parts[at]) != 42 {
		return key
	}
	parts[at] = p.executor.Hex()
	return strings.Join(parts, "|")
}

// insertBounded sets key in m, first evicting the oldest entry when m holds
// limit entries; it returns the evicted key.
func insertBounded(m map[string]time.Time, key string, now time.Time, limit int) (string, bool) {
	if _, ok := m[key]; ok || len(m) < limit {
		m[key] = now
		return "", false
	}
	oldest, at := "", now
	for k, t := range m {
		if !t.After(at) {
			oldest, at = k, t
		}
	}
	delete(m, oldest)
	m[key] = now
	return oldest, true
}

// routeLegs splits a route key into what a pass proves on its own: the funding
// source, and each leg's pool and direction under that funding mode. Every leg
// pays its output to the executor (contracts/src/SwapExecutor.sol), so a leg
// that executed in one route executes the same way in another. The first leg
// of a V3 flash swap pays in its callback and is kept apart. Nil for a key
// that is not a route key.
func routeLegs(key string) []string {
	parts := strings.Split(key, "|")
	if len(parts) < 5 {
		return nil
	}
	executor, funding := parts[0], parts[1]
	legs := []string{"fund|" + strings.Join(parts[:4], "|")}
	for i, leg := range parts[4:] {
		role := funding
		if i == 0 && funding == fmt.Sprint(int(execution.FundingV3FlashSwap)) {
			role += "first"
		}
		legs = append(legs, "leg|"+executor+"|"+role+"|"+leg)
	}
	return legs
}

// count adds delta to the legs of a passed route; the caller holds mu.
func (p *reactivePreflight) count(key string, delta int) {
	if p.legs == nil {
		p.legs = make(map[string]int)
	}
	if whole := wholeOf(key); whole != "" {
		if p.whole == nil {
			p.whole = make(map[string]int)
		}
		if p.whole[whole] += delta; p.whole[whole] <= 0 {
			delete(p.whole, whole)
		}
	}
	for _, leg := range routeLegs(key) {
		if p.legs[leg] += delta; p.legs[leg] <= 0 {
			delete(p.legs, leg)
		}
	}
}

// wholeKey names a route by its funding, start asset and pools in order,
// without the executor and lender of a route key.
func wholeKey(funding execution.Funding, asset domain.Address, pools []domain.PoolKey) string {
	var key strings.Builder
	fmt.Fprintf(&key, "%d|%s", funding, asset.Hex())
	for _, k := range pools {
		fmt.Fprintf(&key, "|%s:%d:%s:%s", k.Address.Hex(), k.Kind, k.PoolID.Hex(), k.Hooks.Hex())
	}
	return key.String()
}

// wholeOf is the wholeKey of a route key; empty for a key that is not one.
func wholeOf(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) < 5 {
		return ""
	}
	whole := parts[1] + "|" + parts[2]
	for _, leg := range parts[4:] {
		pool, _, _ := strings.Cut(leg, ">")
		whole += "|" + pool
	}
	return whole
}

// ran reports whether a route with this funding, start asset and pools passed
// as a whole and has not reverted since. A route proven leg by leg has not
// run: each leg executed, in other routes, at other prices.
func (p *reactivePreflight) ran(funding execution.Funding, asset domain.Address, pools []domain.PoolKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.whole[wholeKey(funding, asset, pools)] > 0
}

// pass remembers a passed route, dropping the oldest pass when full.
func (p *reactivePreflight) pass(key string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.passed == nil {
		p.passed = make(map[string]time.Time)
	}
	if _, ok := p.passed[key]; !ok {
		p.count(key, 1)
	}
	if evicted, ok := insertBounded(p.passed, key, now, preflightPassEntries); ok {
		p.count(evicted, -1)
	}
	// Every leg of a direct pass executed just now.
	delete(p.refuted, key)
	for _, leg := range routeLegs(key) {
		delete(p.refutedLegs, leg)
	}
	p.dirty = true
}

// refute marks a route and its legs as reverted; the caller holds mu.
func (p *reactivePreflight) refute(key string, now time.Time) {
	if p.refuted == nil {
		p.refuted = make(map[string]time.Time)
	}
	if p.refutedLegs == nil {
		p.refutedLegs = make(map[string]time.Time)
	}
	insertBounded(p.refuted, key, now, preflightPassEntries)
	for _, leg := range routeLegs(key) {
		insertBounded(p.refutedLegs, leg, now, preflightPassEntries)
	}
	p.dirty = true
}

// forgotten is what one forget changed, so that restore can undo it. at is
// the time forget stamped on its refutations: a later revert on the route or
// a leg restamps them, and restore leaves those alone.
type forgotten struct {
	key      string
	at       time.Time
	passedAt time.Time // zero when the route held no pass
	route    bool      // the route was not refuted before
	legs     []string  // the legs that were not refuted before
}

// forget drops a route's pass after one of our transactions on it reverted.
func (p *reactivePreflight) forget(intent execution.ReactiveIntent) forgotten {
	p.mu.Lock()
	defer p.mu.Unlock()
	undo := forgotten{key: routeKey(intent), at: time.Now()}
	if at, ok := p.passed[undo.key]; ok {
		undo.passedAt = at
		delete(p.passed, undo.key)
		p.count(undo.key, -1)
		p.dirty = true
	}
	if _, ok := p.refuted[undo.key]; !ok {
		undo.route = true
	}
	for _, leg := range routeLegs(undo.key) {
		if _, ok := p.refutedLegs[leg]; !ok {
			undo.legs = append(undo.legs, leg)
		}
	}
	// The revert does not say which leg failed: none of them composes again
	// until a route with it passes directly, and this route needs its own pass.
	p.refute(undo.key, undo.at)
	return undo
}

// restore undoes a forget whose revert turned out to be a lost race: a rival
// moved a pool first, which says nothing about the route or its legs.
func (p *reactivePreflight) restore(undo forgotten) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, leg := range undo.legs {
		if p.refutedLegs[leg].Equal(undo.at) {
			delete(p.refutedLegs, leg)
		}
	}
	// Another revert on the route since then keeps it refuted and unpassed.
	if !p.refuted[undo.key].Equal(undo.at) {
		p.dirty = true
		return
	}
	if undo.route {
		delete(p.refuted, undo.key)
	}
	if _, ok := p.passed[undo.key]; !ok && !undo.passedAt.IsZero() {
		if p.passed == nil {
			p.passed = make(map[string]time.Time)
		}
		p.count(undo.key, 1)
		if evicted, ok := insertBounded(p.passed, undo.key, undo.passedAt, preflightPassEntries); ok {
			p.count(evicted, -1)
		}
	}
	p.dirty = true
}

const preflightLostEntries = 256

// settled takes the on-chain outcome of one of our transactions. A success
// ran every leg and proves the route. A revert refutes it until diagnosed
// says why: a token may have started refusing, or the route never fit the
// executor.
func (p *reactivePreflight) settled(intent execution.ReactiveIntent, hash domain.Hash, success bool, now time.Time) {
	if success {
		p.pass(routeKey(intent), now)
		p.observePools(intent, routeKey(intent), false)
		return
	}
	undo := p.forget(intent)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost == nil || len(p.lost) >= preflightLostEntries {
		p.lost = make(map[domain.Hash]forgotten)
	}
	p.lost[hash] = undo
}

// diagnosed takes the trace diagnosis of a reverted transaction. Most live
// reverts are the executor's price bound after a rival moved a pool; refuting
// those routes and their legs would leave nearly all later opportunity value
// unproven. The diagnosis names a lost race only
// when the executor itself raised the price bound (exec.revertOrigin).
func (p *reactivePreflight) diagnosed(hash domain.Hash, diagnosis string) {
	p.mu.Lock()
	undo, ok := p.lost[hash]
	delete(p.lost, hash)
	p.mu.Unlock()
	if ok && strings.HasPrefix(diagnosis, "pool_moved:") {
		p.restore(undo)
	}
}

// covers reports whether a Fast intent on an unproven route may be sent
// without a pass: its expected net must cover the configured share of the
// fee budget, the most a trap on the route can burn. The send is the proof:
// settled passes or refutes the route. A route or leg that reverted for us
// is never covered.
func (p *reactivePreflight) covers(intent execution.ReactiveIntent) bool {
	c := intent.Conversion
	if p.unprovenCover == 0 || intent.ExpectedNet == nil || intent.Budget == nil || c.Numerator == nil || c.Numerator.Sign() <= 0 || c.Denominator == nil {
		return false
	}
	key := routeKey(intent)
	p.mu.Lock()
	_, refuted := p.refuted[key]
	for _, leg := range routeLegs(key) {
		if _, bad := p.refutedLegs[leg]; bad {
			refuted = true
		}
	}
	p.mu.Unlock()
	if refuted {
		return false
	}
	// ExpectedNet is in the route's asset; Conversion turns native into it.
	net := new(big.Int).Mul(intent.ExpectedNet, c.Denominator)
	net.Mul(net.Quo(net, c.Numerator), big.NewInt(10000))
	return net.Cmp(new(big.Int).Mul(intent.Budget, new(big.Int).SetUint64(p.unprovenCover))) >= 0
}

func (p *reactivePreflight) diagnose(ctx context.Context, intent execution.ReactiveIntent) ([]tokenrisk.Evidence, tokenrisk.CallFrame, error) {
	block := map[string]any{"blockHash": intent.Version.Anchor.Hash.Hex()}
	var frame tokenrisk.CallFrame
	deadline := time.Now().Add(p.traceWait)
	for {
		err := p.trace.Call(ctx, "debug_traceCall", []any{preflightCall(intent.Call), block, map[string]any{"tracer": "callTracer", "timeout": "2s"}}, &frame)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, frame, fmt.Errorf("trace at quote block: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, frame, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	holds := func(ctx context.Context, token, holder domain.Address, amount *big.Int) (bool, error) {
		var output string
		call := map[string]any{"to": token.Hex(), "data": "0x70a08231" + strings.Repeat("0", 24) + hex.EncodeToString(holder[:])}
		if err := p.trace.Call(ctx, "eth_call", []any{call, block}, &output); err != nil {
			return false, fmt.Errorf("balance of %s: %w", token.Hex(), err)
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(output, "0x"))
		if err != nil || len(raw) != 32 {
			return false, fmt.Errorf("balance of %s: malformed output", token.Hex())
		}
		return new(big.Int).SetBytes(raw).Cmp(amount) >= 0, nil
	}
	payers := map[domain.Address]bool{}
	for _, pool := range routePools(intent) {
		payers[pool.Address] = true
	}
	scope := tokenrisk.Refusal{Executor: intent.Call.Executor, Payers: payers, Protected: p.protected, Holds: holds}
	evidence, err := tokenrisk.DetectRefused(ctx, frame, intent.Version.Anchor, scope)
	if err != nil {
		return nil, frame, err
	}
	// A taxed token reverts the executor's output check (InvalidSwap): HENS
	// keeps 5% of every transfer to it, and without a record every preflight
	// of its routes reverts again.
	routeTokens := map[domain.Address]bool{}
	for _, step := range intent.Route.Steps {
		routeTokens[step.TokenOut] = true
	}
	for _, e := range tokenrisk.Detect(frame, intent.Version.Anchor, intent.Call.Executor) {
		if routeTokens[e.Token] && !p.protected[e.Token] {
			evidence = append(evidence, e)
		}
	}
	// Routes traced before planning saw the exclusion report the same token.
	var recorded []tokenrisk.Evidence
	for _, e := range evidence {
		if p.risks.Check(e.Token) != nil {
			continue
		}
		if err := p.risks.Record(e); err != nil {
			return nil, frame, err
		}
		recorded = append(recorded, e)
	}
	return recorded, frame, nil
}

func (p *reactivePreflight) probeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case intent := <-p.probes:
			if p.claimProbe(intent, time.Now()) {
				p.probe(ctx, intent)
			}
		case m := <-p.measures:
			p.measureGas(ctx, m)
		}
	}
}

// claimProbe checks a route at most once a minute.
func (p *reactivePreflight) claimProbe(intent execution.ReactiveIntent, now time.Time) bool {
	key := routeKey(intent)
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.probed[key]; ok && now.Sub(last) < preflightRetraceAfter {
		return false
	}
	if p.probed == nil || len(p.probed) >= preflightTracedEntries {
		p.probed = make(map[string]time.Time)
	}
	p.probed[key] = now
	return true
}

// probe proves a Fast intent's route once the node has written the Fast
// intent's block, where the opportunity that triggered it still is, unless
// later transactions in that block took it. The route runs with every price
// and output guard removed and a minimum profit of 1, and only a call that
// succeeds proves it: every leg then executed, including the last, which the
// executor's output check precedes. A revert never proves a route (its data
// can come from any contract on the route): an unprofitable or short result
// is inconclusive, and any other revert goes to the trace diagnosis, which
// records refusing and taxed tokens.
func (p *reactivePreflight) probe(ctx context.Context, intent execution.ReactiveIntent) {
	call, err := probeCall(ctx, intent)
	if err != nil {
		slog.Warn("fast route probe not encoded", "intent", intent.ID.Hex(), "error", err)
		return
	}
	block, ok := p.writtenBlock(ctx, intent.Version.BlockNumber)
	if !ok {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var output string
	err = p.read.Call(callCtx, "eth_call", []any{preflightCall(call), map[string]any{"blockHash": block.Hash.Hex()}}, &output)
	var rpcErr *rpc.RPCError
	switch {
	case err == nil:
		p.pass(routeKey(intent), time.Now())
		p.observePools(intent, routeKey(intent), false)
		// The probe's guards are removed; they read a few slots, a small
		// share of the route's gas.
		p.queueMeasure(intent, call, block.Hash)
	case !errors.As(err, &rpcErr) || (rpcErr.Code != 3 && !strings.Contains(rpcErr.Message, "execution reverted")):
	case revertSelector(rpcErr) == insufficientProfit || revertSelector(rpcErr) == slippage:
	case p.trace != nil:
		probed := intent
		probed.Call, probed.Version.Anchor = call, block
		select {
		case p.reverts <- probed:
		default:
		}
	}
}

// probeCall is intent's call with every price and output guard removed and a
// minimum profit of 1.
func probeCall(ctx context.Context, intent execution.ReactiveIntent) (execution.ExecutorCall, error) {
	plan := intent.Route
	plan.MinRate, plan.MinProfit, plan.Deadline = nil, big.NewInt(1), 1<<40
	plan.Steps = append([]execution.ExecutorStep(nil), plan.Steps...)
	for i := range plan.Steps {
		plan.Steps[i].PriceBound, plan.Steps[i].MinAmountOut = nil, big.NewInt(1)
	}
	return execution.BuildExecutorCall(ctx, plan, intent.Call.CallParameters)
}

// writtenBlock waits up to a second for the node to serve block number.
func (p *reactivePreflight) writtenBlock(ctx context.Context, number uint64) (domain.BlockRef, bool) {
	deadline := time.Now().Add(time.Second)
	for {
		block, err := p.read.ExecutionBlock(ctx, fmt.Sprintf("0x%x", number))
		if err == nil {
			return block.Ref, true
		}
		if !errors.Is(err, rpc.ErrBlockUnavailable) || time.Now().After(deadline) {
			return domain.BlockRef{}, false
		}
		select {
		case <-ctx.Done():
			return domain.BlockRef{}, false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func revertSelector(err *rpc.RPCError) string {
	data := strings.Trim(string(err.Data), `"`)
	if len(data) < 10 {
		return ""
	}
	return strings.ToLower(data[:10])
}

// gasMeasure is a passed route's call and the block it passed at; a zero
// block is a Fast intent's, measured at its written block.
type gasMeasure struct {
	intent execution.ReactiveIntent
	block  domain.Hash
}

// queueMeasure asks for call's gas at block without waiting: the trace runs on
// the probe loop, off the send path.
func (p *reactivePreflight) queueMeasure(intent execution.ReactiveIntent, call execution.ExecutorCall, block domain.Hash) {
	if p.gas == nil || p.trace == nil {
		return
	}
	key, now := routeKey(intent), time.Now()
	p.mu.Lock()
	if last, ok := p.measured[key]; ok && now.Sub(last) < measureRouteAfter {
		p.mu.Unlock()
		return
	}
	if p.measured == nil || len(p.measured) >= preflightTracedEntries {
		p.measured = make(map[string]time.Time)
	}
	p.measured[key] = now
	p.mu.Unlock()
	intent.Call = call
	select {
	case p.measures <- gasMeasure{intent: intent, block: block}:
	default:
	}
}

// measureGas traces a passed route's call and records the gas it used. Only
// the top frame is needed; a reverted trace measures nothing.
func (p *reactivePreflight) measureGas(ctx context.Context, m gasMeasure) {
	if m.block == (domain.Hash{}) {
		// The probe's guards are removed; they read a few slots, a small
		// share of the route's gas.
		block, ok := p.writtenBlock(ctx, m.intent.Version.BlockNumber)
		if !ok {
			return
		}
		call, err := probeCall(ctx, m.intent)
		if err != nil {
			return
		}
		m.intent.Call, m.block = call, block.Hash
	}
	var frame struct {
		GasUsed string `json:"gasUsed"`
		Error   string `json:"error"`
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	config := map[string]any{"tracer": "callTracer", "tracerConfig": map[string]any{"onlyTopCall": true}, "timeout": "1s"}
	if err := p.trace.Call(ctx, "debug_traceCall", []any{preflightCall(m.intent.Call), map[string]any{"blockHash": m.block.Hex()}, config}, &frame); err != nil || frame.Error != "" {
		return
	}
	used, err := strconv.ParseUint(strings.TrimPrefix(frame.GasUsed, "0x"), 16, 64)
	if err != nil || used == 0 {
		return
	}
	// The traced call ran the whole route at a written block.
	p.pass(routeKey(m.intent), time.Now())
	p.gas(planPools(m.intent.Route), m.intent.Route.Funding, used)
}
