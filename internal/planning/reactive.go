package planning

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

// ReactiveCosts is a cached, timestamped envelope. Extra includes OP L1 and
// operator fees. Nitro charges its L1 cost as poster gas inside the gas
// limit instead, priced by Poster.
type ReactiveCosts struct {
	BaseFee, Extra *big.Int
	ExpiresMS      int64
	// L1 and Operator, when measured, size the reserve to a route instead of
	// the worst case in Extra: L1 by unsigned transaction size, Operator by gas.
	L1, Operator *LinearFee
	Poster       *PosterPrice
	// ConfigVersion is the executor's verified, unpaused configVersion; hot
	// routes condition on it (HotConditions). Zero sends none.
	ConfigVersion uint64
	// ClaimEpoch is the hot executor's claimEpoch, which racing variants
	// hold; zero when unread.
	ClaimEpoch uint64
	// Inventory is the executor's own balance of each start asset funded
	// from it (assets[].inventory); routes may spend it as their principal.
	Inventory map[domain.Address]*big.Int
}

// PosterPrice is Nitro's L1 price for a transaction: ArbGasInfo's
// getPricesInWei per-transaction and per-calldata-byte charges.
type PosterPrice struct {
	PerTx, PerByte *big.Int
}

// PosterGas is the gas a Nitro chain adds to a route's transaction for its L1
// cost: the cost at the uncompressed size bound, which brotli only shrinks,
// with twofold headroom for an L1 price or base fee that moves before
// inclusion, in units of the current base fee. Zero without a Poster price.
func (c ReactiveCosts) PosterGas(steps int) uint64 {
	return c.PosterGasAt(execution.PackedCalldataBound(steps) + 200)
}

// PosterGasAt is PosterGas for a signed transaction of at most size bytes.
func (c ReactiveCosts) PosterGasAt(size int) uint64 {
	if c.Poster == nil || c.BaseFee == nil || c.BaseFee.Sign() <= 0 {
		return 0
	}
	bytes := big.NewInt(int64(size))
	cost := new(big.Int).Add(c.Poster.PerTx, bytes.Mul(bytes, c.Poster.PerByte))
	cost.Mul(cost, big.NewInt(2))
	gas := cost.Add(cost, new(big.Int).Sub(c.BaseFee, big.NewInt(1))).Quo(cost, c.BaseFee)
	if !gas.IsUint64() {
		return math.MaxUint64
	}
	return gas.Uint64()
}

// LinearFee interpolates a fee measured at two inputs, rounding up. OP's data
// and operator fee functions are linear, or convex with a floor, in their
// input, so the chord bounds them from above between the samples.
type LinearFee struct {
	X0, X1 uint64
	Y0, Y1 *big.Int
}

func (f *LinearFee) At(x uint64) *big.Int {
	if f.X1 <= f.X0 {
		return new(big.Int).Set(f.Y1)
	}
	slope := new(big.Int).Sub(f.Y1, f.Y0)
	slope.Mul(slope, new(big.Int).Sub(new(big.Int).SetUint64(x), new(big.Int).SetUint64(f.X0)))
	span := new(big.Int).SetUint64(f.X1 - f.X0)
	slope.Add(slope, new(big.Int).Sub(span, big.NewInt(1))) // round up
	slope.Div(slope, span)
	return slope.Add(slope, f.Y0)
}

// ExtraFor is the data and operator fee reserve for a route of steps hops and
// the given gas limit, with the same twofold headroom as the cached worst case.
func (c ReactiveCosts) ExtraFor(steps int, gas uint64) *big.Int {
	if c.L1 == nil || c.Operator == nil {
		return c.Extra
	}
	size, err := execution.OPL1SizeBound(execution.PackedCalldataBound(steps) + 200)
	if err != nil {
		return c.Extra
	}
	extra := new(big.Int).Add(c.L1.At(size), c.Operator.At(gas))
	extra.Mul(extra, big.NewInt(2))
	if extra.Cmp(c.Extra) > 0 {
		return c.Extra
	}
	return extra
}

// lenderBalanceMultiple is how many times the principal a V3 flash lender
// must hold. A lender holding less than the loan (the cheapest Arbitrum One
// WETH lender can hold a small fraction of it) fails every preflight in its
// transfer.
const lenderBalanceMultiple = 2

type ReactivePolicy struct {
	TokenCheck func(domain.Address) error
	PoolCheck  func(domain.PoolKey) error
	// LenderBalance is a V3 flash lender's balance of a token at the last
	// canonical block; nil admits every lender, as before balances were
	// tracked. A lender lends only with lenderBalanceMultiple times the
	// principal, and not at all while its balance is unknown.
	LenderBalance             func(lender domain.PoolKey, token domain.Address) (*big.Int, bool)
	MinimumFeeCap, MinimumTip *big.Int
	GasByFunding              map[execution.Funding]uint64
	Chain                     domain.ChainIdentity
	// Ordering is the sequencer's policy. Under arrival ordering a tip buys
	// no position, so intents sign the minimum tip.
	Ordering                                           domain.Ordering
	Sender, Executor                                   domain.Address
	CodeHash                                           domain.Hash
	GasLimit                                           uint64
	MaxFee, MaxTip, TargetTip, MaxTotalFee, MinimumNet *big.Int
	BidShareBPS, QuoteReserveBPS, SlippageBPS          uint64
	Lifetime                                           time.Duration
	// RouteGasBase and RouteGasPerStep bound a route's gas by its length when
	// set; GasLimit and GasByFunding remain ceilings.
	RouteGasBase, RouteGasPerStep uint64
	// BaseFeeHeadroomBPS is the fee cap's multiple of the current base fee,
	// in basis points; zero means 20000 (twice). The budget and profit guard
	// charge the whole gas limit at the fee cap, so on a chain whose base fee
	// sits at its floor a smaller headroom admits routes the doubled cap
	// rejects. A base fee that rises past the cap loses the send, not a fee:
	// the sequencer refuses it at submission.
	BaseFeeHeadroomBPS uint64
	// DeadlineSeconds is the on-chain deadline after the build time. The
	// executor's profit check, not the deadline, protects a late inclusion.
	DeadlineSeconds uint64
	// PricePrecheck bounds every step's pool state (SetPriceBounds), so a
	// transaction that lost its pools to a rival reverts before swapping.
	// The configured executor must decode packed version 3.
	PricePrecheck bool
	// CumulativePrecheck, with PricePrecheck, checks the product of the legs'
	// rate changes instead of each leg (SetPriceReferences, packed version 4). The configured executor must decode version 4.
	CumulativePrecheck bool
	// MetricOmm admits routes through MetricOmm pools. The configured
	// executor must have the MetricOmm leg (kind 8); an older one
	// rejects the route on chain and the revert is paid for.
	MetricOmm bool
	// WrapNative admits routes with a wrap leg, a deposit into or a withdrawal
	// from the wrapped native token. The configured executor must have the
	// WRAP leg (kind 9); an older one rejects the route on chain and
	// the revert is paid for.
	WrapNative bool
	// HotPath sends V3-family flash-swap routes to executeV3Hot as conditional
	// transactions. The conditions hold the executor's slot 0 and
	// the deadline; every leg keeps a one-sided price bound, as SetPriceBounds
	// sets it. The configured executor must have executeV3Hot and the
	// sequencer must evaluate conditions at the execution position.
	HotPath bool
	// HotChain says the configured executor has executeHot: hot routes
	// go out in its chained form, which also runs V2 pairs and a Metric pool
	// held by conditions. An executor without it rejects the call on chain.
	HotChain bool
	// RouteRan, when set, reports whether a route with this funding, start
	// asset and pools ever passed as a whole: simulated, traced or settled.
	// One that did not, and a route with a pool no condition holds, tip a
	// bounded number of base fees (cautiousTip).
	RouteRan func(funding execution.Funding, asset domain.Address, pools []domain.PoolKey) bool
	// QuietPool, on a hot route, returns the storage conditions that hold a
	// pool unchanged since the quote, or nil. A held pool drops its price
	// bound: a rival that moves it first voids the transaction free.
	QuietPool func(pool.State) []execution.SlotCondition
	// PackedConditions sends other routes conditionally too when quiet pools
	// can be held, keeping the packed precheck; a rival that moves a held
	// pool first voids them free.
	PackedConditions bool
	// SkipTaxedPools rejects routes through a Slipstream pool with a swap hook
	// or an MEV tax module: the tax grows with the tip, and the hook reserves
	// slipstreamHookGas on top of the route's limit.
	SkipTaxedPools bool
	// TipGas, when set, gives the gas the profit guard reserves the tip on for
	// a route, its funding and gas limit, clamped to [1, limit]; nil reserves
	// it on the whole limit. The priority fee is charged on gas used, which is
	// about half the limit.
	TipGas func(route []domain.PoolKey, funding execution.Funding, limit uint64) uint64
	// Bid, when set, prices the priority fee from observed competition on the
	// route, given the native surplus after base costs, the gas limit and the
	// highest tip the surplus, fee cap and transaction budget allow. A decision
	// that is not Applied is recorded only; the share-of-surplus tip is signed.
	// An Applied decision with a Reason, or without positive expected profit
	// (a probe may break even), stops the build.
	Bid        func(BidRequest) *execution.BidDecision
	ReceivedAt time.Time
	Lane       string
}

// BuildReactive evaluates the conservative fee envelope for each funding
// choice. Inputs and RPC observations are supplied by the runtime; this path
// performs no network requests and does not fabricate execution evidence.
// baseFeeHeadroom is the fee cap over the base fee, in basis points.
func baseFeeHeadroom(cfg ReactivePolicy) uint64 {
	if cfg.BaseFeeHeadroomBPS == 0 {
		return 20000
	}
	return cfg.BaseFeeHeadroomBPS
}

func baseFeeWithHeadroom(baseFee *big.Int, headroom uint64) *big.Int {
	base := new(big.Int).Mul(baseFee, new(big.Int).SetUint64(headroom))
	return base.Add(base, big.NewInt(9999)).Quo(base, big.NewInt(10000))
}

// WrapLegGas is the gas a wrap leg is expected to use: the executor's call
// to the wrapped native token, the token's balance update and the ether
// transfer. UNVERIFIED: an estimate from the opcode costs (a cold account
// and proxy, one balance slot, a value transfer), to be replaced by the gas
// of traced wrap legs on Arbitrum One once routes with one have run. It
// prices a route whose kinds were never measured; the gas limit still counts
// a wrap as a whole step.
const WrapLegGas = 45_000

// routeGasLimit is the gas limit of a route of the given length and funding.
func routeGasLimit(cfg ReactivePolicy, pools int, funding execution.Funding, hooked bool, poster uint64) uint64 {
	limit := cfg.GasLimit
	if units := cfg.GasByFunding[funding]; units >= 21000 && units <= cfg.GasLimit {
		limit = units
	}
	if cfg.RouteGasPerStep > 0 {
		limit = min(limit, cfg.RouteGasBase+cfg.RouteGasPerStep*uint64(pools))
	}
	if hooked {
		limit = min(limit+slipstreamHookGas, cfg.GasLimit)
	}
	// Nitro's gas limit must also cover poster gas; the policy limit
	// bounds execution only, the total fee caps bound the cost.
	return limit + poster
}

// expectedRouteGas is what a route is expected to use of its limit.
func expectedRouteGas(cfg ReactivePolicy, pools []domain.PoolKey, funding execution.Funding, limit uint64) uint64 {
	if cfg.TipGas == nil {
		return limit
	}
	return min(max(cfg.TipGas(pools, funding, limit), 1), limit)
}

// NetworkCostFloor is the least native execution budget BuildReactive charges
// a route through these pools: the cheapest funding mode, no swap hook, no
// tip. A route whose gross is worth less in native cannot be built, and the
// caller can drop it without the funding and bid work of a build.
func NetworkCostFloor(pools []domain.PoolKey, costs ReactiveCosts, cfg ReactivePolicy) *big.Int {
	if costs.BaseFee == nil || costs.BaseFee.Sign() <= 0 {
		return new(big.Int)
	}
	base := baseFeeWithHeadroom(costs.BaseFee, baseFeeHeadroom(cfg))
	poster := costs.PosterGas(len(pools))
	var floor *big.Int
	for _, funding := range []execution.Funding{execution.FundingWallet, execution.FundingV3Flash, execution.FundingV3FlashSwap, execution.FundingV4Flash, execution.FundingInventory} {
		gas := routeGasLimit(cfg, len(pools), funding, false, poster)
		budget := new(big.Int).Mul(base, new(big.Int).SetUint64(expectedRouteGas(cfg, pools, funding, gas)))
		if extra := costs.ExtraFor(len(pools), gas); extra != nil {
			budget.Add(budget, extra)
		}
		if floor == nil || budget.Cmp(floor) < 0 {
			floor = budget
		}
	}
	return floor
}

func BuildReactive(ctx context.Context, view arb.View, o arb.Opportunity, lenders []pool.State, wallet *big.Int,
	version domain.StateVersion, conversion execution.FeeConversion, costs ReactiveCosts, cfg ReactivePolicy, now time.Time) (execution.ReactiveIntent, error) {
	if costs.BaseFee == nil || costs.BaseFee.Sign() <= 0 || costs.Extra == nil || costs.Extra.Sign() < 0 || costs.ExpiresMS <= now.UnixMilli() {
		return execution.ReactiveIntent{}, fmt.Errorf("reactive fee context unavailable")
	}
	if cfg.GasLimit < 21000 || cfg.DeadlineSeconds < 2 || cfg.DeadlineSeconds > 30 || cfg.MaxFee == nil || cfg.MaxTip == nil || cfg.TargetTip == nil || cfg.MinimumNet == nil || cfg.MinimumNet.Sign() <= 0 || cfg.BidShareBPS > 10000 || cfg.QuoteReserveBPS >= 10000 || cfg.Lifetime <= 0 || cfg.Lifetime > 2*time.Second {
		return execution.ReactiveIntent{}, fmt.Errorf("invalid reactive policy")
	}
	if cfg.SkipTaxedPools && routeHasTaxedPool(view, o) {
		return execution.ReactiveIntent{}, fmt.Errorf("taxed pool")
	}
	if err := checkMetricQuotes(view, o, cfg.MetricOmm, now); err != nil {
		return execution.ReactiveIntent{}, err
	}
	if !cfg.WrapNative && hasWrapLeg(o.Cycle.Pools) {
		return execution.ReactiveIntent{}, fmt.Errorf("wrap leg not enabled for the executor")
	}
	if cfg.PoolCheck != nil {
		// Drop quarantined lenders here so another lender can fund the route.
		lenders = slices.DeleteFunc(slices.Clone(lenders), func(p pool.State) bool { return cfg.PoolCheck(p.Key) != nil })
	}
	if cfg.LenderBalance != nil && o.Input != nil {
		// The canonical balance can trail the frame a block, and earlier
		// transactions of our block can draw on the same lender.
		needed := new(big.Int).Mul(o.Input, big.NewInt(lenderBalanceMultiple))
		lenders = slices.DeleteFunc(slices.Clone(lenders), func(p pool.State) bool {
			balance, ok := cfg.LenderBalance(p.Key, o.Cycle.Start)
			return !ok || balance == nil || balance.Cmp(needed) < 0
		})
	}
	if HasTransactionFees(o) {
		var err error
		o, _, err = QuoteTransactionBound(ctx, view, o, new(big.Int))
		if err != nil {
			return execution.ReactiveIntent{}, err
		}
	}
	choices := FundingChoices(o, lenders, wallet, costs.Inventory[o.Cycle.Start])
	if len(choices) == 0 {
		return execution.ReactiveIntent{}, fmt.Errorf("no funded execution path")
	}
	conservative := o
	// Quote error scales with the exchanged amount. Reserving a percentage of
	// the small gross profit can leave almost all principal exposure uncovered.
	output := new(big.Int).Add(o.Input, o.Net)
	reserve := new(big.Int).Mul(output, new(big.Int).SetUint64(cfg.QuoteReserveBPS))
	reserve.Add(reserve, big.NewInt(9999)).Quo(reserve, big.NewInt(10000))
	conservative.Net = new(big.Int).Sub(o.Net, reserve)
	// Base-fee headroom and priority fee have distinct budgets. Adding the bid
	// to the fee cap avoids silently capping it at one extra base fee.
	headroom := baseFeeHeadroom(cfg)
	base := baseFeeWithHeadroom(costs.BaseFee, headroom)
	hooked := routeHasSwapHook(view, o)
	poster := costs.PosterGas(len(o.Cycle.Pools))
	gasFor := func(a AssetPolicy) uint64 { return routeGasLimit(cfg, len(o.Cycle.Pools), a.Funding, hooked, poster) }
	// expectedGas is what a route is expected to use: base fees are charged on
	// it, not on the limit, which only bounds the worst case. Routes
	// typically use about half their limit (two hops: 262k–397k of 757k).
	expectedGas := func(funding execution.Funding, limit uint64) uint64 {
		return expectedRouteGas(cfg, o.Cycle.Pools, funding, limit)
	}
	// The execution budget depends on the funding mode alone; a start asset
	// can have dozens of V3 lenders that would each recompute it. ChooseFunding
	// only reads the returned value.
	budgets := map[execution.Funding]*big.Int{}
	choice, err := ChooseFunding(ctx, conservative, choices, cfg.MinimumNet, func(_ context.Context, a AssetPolicy) (*big.Int, error) {
		if budget, ok := budgets[a.Funding]; ok {
			return budget, nil
		}
		gas := gasFor(a)
		used := new(big.Int).SetUint64(expectedGas(a.Funding, gas))
		zeroBudget := new(big.Int).Add(new(big.Int).Mul(base, used), costs.ExtraFor(len(o.Cycle.Pools), gas))
		budget, err := execution.FeeInAsset(zeroBudget, conversion)
		if err == nil {
			budgets[a.Funding] = budget
		}
		return budget, err
	})
	if err != nil {
		return execution.ReactiveIntent{}, err
	}
	gas := new(big.Int).SetUint64(gasFor(choice.Asset))
	tipGas := expectedGas(choice.Asset.Funding, gas.Uint64())
	surplus := new(big.Int).Sub(choice.Net, cfg.MinimumNet)
	// Convert surplus back to native, rounding down, then reserve a configurable
	// share for bidding. The remainder absorbs model error and opportunity decay.
	native := new(big.Int).Mul(surplus, conversion.Denominator)
	native.Quo(native, conversion.Numerator)
	tip := new(big.Int).Mul(native, new(big.Int).SetUint64(cfg.BidShareBPS))
	tip.Quo(tip, big.NewInt(10000))
	tip.Quo(tip, new(big.Int).SetUint64(tipGas))
	if tip.Cmp(cfg.TargetTip) > 0 {
		tip.Set(cfg.TargetTip)
	}
	if cfg.Ordering == domain.OrderingArrival {
		tip.SetInt64(0) // raised to MinimumTip below
	}
	var bid *execution.BidDecision
	var priced map[uint64]pricedReactiveQuote
	if cfg.Bid != nil {
		extra := costs.ExtraFor(len(o.Cycle.Pools), gas.Uint64())
		ceiling := tipCeiling(cfg, native, base, extra, gas, tipGas)
		if cfg.Ordering == domain.OrderingArrival {
			ceiling.SetInt64(0)
			if cfg.MinimumTip != nil {
				ceiling.Set(cfg.MinimumTip)
			}
		}
		var shareTip *big.Int
		if fees, byValue := cautiousTip(cfg, view, o, choice.Asset.Funding); fees != 0 {
			bound := new(big.Int).Mul(costs.BaseFee, big.NewInt(fees))
			if share := valueShareTip(native, tipGas); byValue && share != nil && share.Cmp(bound) > 0 {
				bound = share
			}
			if ceiling.Cmp(bound) > 0 {
				ceiling.Set(bound)
			}
			if byValue && native.Cmp(valueShareSurplus) >= 0 {
				shareTip = new(big.Int).Set(ceiling)
			}
		}
		reserved := new(big.Int).Mul(base, new(big.Int).SetUint64(tipGas))
		request := BidRequest{Route: o.Cycle.Pools, Funding: choice.Asset.Funding, Amount: o.Input, Surplus: native, Gas: gas.Uint64(), Base: costs.BaseFee, Extra: extra, Ceiling: ceiling, MinimumTip: cfg.MinimumTip, ReceivedAt: cfg.ReceivedAt, Lane: cfg.Lane, Reserved: reserved}
		request.ShareTip = shareTip
		request.LossGas, request.Conditional = lossGas(cfg, view, o, choice.Asset.Funding, costs)
		if HasTransactionFees(o) {
			request.SurplusAtTip, priced = transactionBidCosts(ctx, view, o, choice, conversion, cfg, base, extra, gas.Uint64(), tipGas)
		}
		bid = cfg.Bid(request)
		if bid != nil && bid.Applied {
			// No colon: the skip counter keys on the text before one.
			if bid.Reason != "" {
				return execution.ReactiveIntent{}, fmt.Errorf("bid %s", bid.Reason)
			}
			if bid.Expected.Sign() < 0 || bid.Expected.Sign() == 0 && !bid.Probe {
				return execution.ReactiveIntent{}, fmt.Errorf("bid %s", BidNoProfit)
			}
			tip.Set(bid.ModelTip)
		}
	}
	if tip.Cmp(cfg.MaxTip) > 0 {
		tip.Set(cfg.MaxTip)
	}
	if cfg.MinimumTip != nil && tip.Cmp(cfg.MinimumTip) < 0 {
		tip.Set(cfg.MinimumTip)
	}
	if tip.Cmp(cfg.MaxTip) > 0 {
		return execution.ReactiveIntent{}, fmt.Errorf("replacement tip exceeds policy")
	}
	if !HasTransactionFees(o) {
		intent, err := buildReactiveBid(ctx, view, o, nil, reserve, choice, conversion, costs, cfg, base, gas.Uint64(), tipGas, tip, version, now)
		return withBid(ctx, intent, bid, err)
	}
	if bid != nil && bid.Applied && bid.CostVersion != "" {
		quoted, ok := priced[tip.Uint64()]
		if !ok || tip.Cmp(bid.ModelTip) != 0 {
			return execution.ReactiveIntent{}, fmt.Errorf("bid economics differ from signed tip")
		}
		intent, err := buildReactiveBid(ctx, view, quoted.opportunity, quoted.fees, quoted.reserve, choice, conversion, costs, cfg, base, gas.Uint64(), tipGas, tip, version, now)
		return withBid(ctx, intent, bid, err)
	}
	// A larger bid can increase both network cost and pool tax. Try a bounded
	// descending ladder, retaining the highest tested bid that remains profitable.
	floor := new(big.Int)
	if cfg.MinimumTip != nil {
		floor.Set(cfg.MinimumTip)
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		quoted, fees, err := QuoteTransactionBound(ctx, view, o, tip)
		if err != nil {
			return execution.ReactiveIntent{}, err
		}
		adjustedReserve := new(big.Int).Add(quoted.Input, quoted.Net)
		adjustedReserve.Mul(adjustedReserve, new(big.Int).SetUint64(cfg.QuoteReserveBPS))
		adjustedReserve.Add(adjustedReserve, big.NewInt(9999)).Quo(adjustedReserve, big.NewInt(10000))
		intent, err := buildReactiveBid(ctx, view, quoted, fees, adjustedReserve, choice, conversion, costs, cfg, base, gas.Uint64(), tipGas, tip, version, now)
		if err == nil {
			return withBid(ctx, intent, bid, nil)
		}
		lastErr = err
		if tip.Cmp(floor) == 0 {
			break
		}
		tip = new(big.Int).Quo(tip, big.NewInt(2))
		if attempt == 2 || tip.Cmp(floor) < 0 {
			tip.Set(floor)
		}
	}
	return execution.ReactiveIntent{}, lastErr
}

// tipCeiling is the highest tip a build can sign: the policy maximum, the fee
// cap above the base-fee headroom, the transaction budget, and the surplus.
// The budget covers the whole gas limit; the profit guard reserves the tip on
// tipGas, so the surplus bounds it per unit of tipGas.
// hotSupported reports whether the configured executor's hot entry point runs
// route: executeHot's chained form with HotChain, executeV3Hot's otherwise.
func hotSupported(cfg ReactivePolicy, route execution.ExecutorPlan) bool {
	if cfg.HotChain {
		return execution.HotChainSupported(route)
	}
	return execution.HotRouteSupported(route)
}

// chainChecked reports whether every pair and Metric pool of a chained hot
// route is held by a condition, after holdQuietPools. The chained route sends
// those pools their quoted amounts and reads no price of theirs, so one that
// moved would revert for the whole route's gas; only a V3-family pool can
// carry a price bound instead.
func chainChecked(view arb.View, route execution.ExecutorPlan) bool {
	for _, step := range route.Steps {
		switch step.Pool.Kind {
		case domain.PoolKindV2:
			if step.PriceBound != nil {
				return false
			}
		case domain.PoolKindMetricOmm:
			state, ok := view.Pool(step.Pool)
			if !ok || state.Metric == nil || len(state.Metric.Words) == 0 {
				return false
			}
		}
	}
	return true
}

// stateless reports a pool with no state a rival could move: the wrap of the
// native token converts 1:1 whatever came before it. Such a pool counts as
// held, with no condition, wherever a route needs every pool held.
func stateless(key domain.PoolKey) bool {
	return key.Kind == domain.PoolKindWrap
}

// hasWrapLeg reports a route through the wrapped native token's own pool.
func hasWrapLeg(pools []domain.PoolKey) bool {
	return slices.ContainsFunc(pools, func(key domain.PoolKey) bool { return key.Kind == domain.PoolKindWrap })
}

// holdQuietPools adds each step's pool that quiet holds to the conditions.
// On a hot route the condition replaces the step's price bound; a packed
// route keeps its precheck, whose cumulative rate spans every step.
func holdQuietPools(view arb.View, route *execution.ExecutorPlan, conditions *execution.TransactionConditions, quiet func(pool.State) []execution.SlotCondition, dropBounds bool) {
	if quiet == nil {
		return
	}
	for i := range route.Steps {
		step := &route.Steps[i]
		state, ok := view.Pool(step.Pool)
		if !ok || stateless(step.Pool) {
			continue
		}
		if metric := state.Metric; step.Pool.Kind == domain.PoolKindMetricOmm && metric != nil && len(metric.Words) != 0 {
			// The leg's price is the pool's bin and position (slot 0) and
			// the oracle's last update. It has no price precheck, so a swap
			// ahead of us would be found only by the profit check at the
			// end of the route, for the whole route's gas: slot 0 is held.
			// The oracle's word is not: it changes about every 2 s, and the
			// sequencer admits a condition only if it held 2 s earlier, so
			// holding it had nearly every such send rejected.
			conditions.Hold(step.Pool.Address, []execution.SlotCondition{{Value: metric.Words[0]}})
			continue
		}
		slots := quiet(state)
		if len(slots) == 0 {
			continue
		}
		if dropBounds {
			step.PriceBound = nil
		}
		conditions.Hold(step.Pool.Address, slots)
	}
}

// firstRunTipFees is the tip, in base fees, a route may bid before it has
// run as a whole when every pool is held. Rivals' median tip on contested
// routes is about one base fee (1.86e7 wei per gas at a base fee of 2e7);
// five base fees clear it, and a full revert at that tip costs
// six times the route's base cost, about $0.10 at 400,000 gas.
const firstRunTipFees = 5

// valueShareSurplus is the native surplus, about $0.08, from which a route
// with an unheld pool behind a price bound bids half its worth. Rivals on
// the same route typically tip 16%, 49% and 75% of the worth (quartiles), so
// a tip of 1% to 12% under a cap in base fees rarely wins. A higher threshold
// (7.5e13) leaves sends worth $0.087 to $0.140 at one base fee, where rivals
// tip 7 to 21; a loss at the bound costs 32,000 to 57,000 gas, about $0.01
// at this tip.
var valueShareSurplus = big.NewInt(40_000_000_000_000)

// valueShareTip is half of surplus per unit of tipGas, or nil below
// valueShareSurplus. Of the sends that lost that night 11% ran the whole
// route, so a loss at this tip costs about a tenth of the surplus on
// average and the whole tip, half the surplus, at worst.
func valueShareTip(surplus *big.Int, tipGas uint64) *big.Int {
	if surplus.Cmp(valueShareSurplus) < 0 || tipGas == 0 {
		return nil
	}
	return new(big.Int).Quo(surplus, new(big.Int).SetUint64(2*tipGas))
}

// cautiousTip is the most a route may tip, in base fees, or 0 for no bound
// beyond the usual ceiling, and whether valueShareTip may raise that bound.
// The tip is charged on the gas a revert uses, so a send that runs its swaps
// and then fails the profit check pays it on the whole route. A pool no
// condition holds allows one base fee, as does a chain without conditions.
// When that pool has a price bound and the route has run as a whole, a
// rival's trade usually fails the bound at 32,000 to 56,000 gas, and the
// bound may follow the route's value instead; an unknown pool, a MetricOmm
// leg (held at its position only: a newer oracle quote ahead of us is paid
// for) and a packed route without the precheck have no such bound. A route
// that has not run as a whole may have a wrong quote (a pool state held
// wrongly, a token that keeps part of a transfer) and fails with every pool
// unchanged: firstRunTipFees while every pool is held.
func cautiousTip(cfg ReactivePolicy, view arb.View, o arb.Opportunity, funding execution.Funding) (fees int64, byValue bool) {
	ran := cfg.RouteRan == nil || cfg.RouteRan(funding, o.Cycle.Start, o.Cycle.Pools)
	if cfg.QuietPool == nil {
		if !ran {
			return 1, false
		}
		return 0, false
	}
	unheld := false
	for _, key := range o.Cycle.Pools {
		if stateless(key) {
			continue
		}
		state, ok := view.Pool(key)
		if !ok {
			return 1, false
		}
		if len(cfg.QuietPool(state)) != 0 {
			continue
		}
		// A packed route has price bounds only with the precheck.
		if key.Kind == domain.PoolKindMetricOmm || !cfg.PricePrecheck && !cfg.HotPath {
			return 1, false
		}
		unheld = true
	}
	if unheld {
		return 1, ran
	}
	if !ran {
		return firstRunTipFees, false
	}
	return 0, false
}

func tipCeiling(cfg ReactivePolicy, native, base, extra, gas *big.Int, tipGas uint64) *big.Int {
	ceiling := new(big.Int).Set(cfg.MaxTip)
	bounds := []*big.Int{new(big.Int).Sub(cfg.MaxFee, base), new(big.Int).Quo(native, new(big.Int).SetUint64(tipGas))}
	if cfg.MaxTotalFee != nil {
		budget := new(big.Int).Sub(cfg.MaxTotalFee, extra)
		bounds = append(bounds, budget.Quo(budget, gas).Sub(budget, base))
	}
	for _, bound := range bounds {
		if bound.Cmp(ceiling) < 0 {
			ceiling.Set(bound)
		}
	}
	if ceiling.Sign() < 0 {
		ceiling.SetInt64(0)
	}
	return ceiling
}

// withBid attaches the competition model's decision and reseals the intent,
// since its identity covers every recorded field.
func withBid(ctx context.Context, intent execution.ReactiveIntent, bid *execution.BidDecision, err error) (execution.ReactiveIntent, error) {
	if err != nil || bid == nil {
		return intent, err
	}
	intent.Bid = bid
	return execution.SealReactiveIntent(ctx, intent)
}

// slipstreamHookGas is reserved once for a route through a Slipstream pool
// with a swap hook. DynamicSwapFeeHook.afterSwap requires gasleft() above its
// recordCallGasLimit (250,000 on OP Mainnet) and reverts "IG"
// otherwise. Swaps nest in their callbacks and every call frame keeps 1/64
// back, so the deepest swap sees the least gas: routes crossing more ticks
// than usual reverted after spending about a million gas.
const slipstreamHookGas = 300_000

// routeHasSwapHook reports whether a route pool runs a Slipstream swap hook.
// Legacy pools have a fee module instead, which has no afterSwap.
func routeHasSwapHook(view arb.View, o arb.Opportunity) bool {
	for _, key := range o.Cycle.Pools {
		p, ok := view.Pool(key)
		if ok && p.Slipstream != nil && !p.Slipstream.Legacy && p.Slipstream.Hook != (domain.Address{}) {
			return true
		}
	}
	return false
}

// routeHasTaxedPool reports whether a route pool charges a tip-dependent MEV
// tax or runs a swap hook. Legacy pools have neither.
func routeHasTaxedPool(view arb.View, o arb.Opportunity) bool {
	for _, key := range o.Cycle.Pools {
		p, ok := view.Pool(key)
		if ok && p.Slipstream != nil && !p.Slipstream.Legacy && (p.Slipstream.Hook != (domain.Address{}) || p.Slipstream.TaxModule != (domain.Address{})) {
			return true
		}
	}
	return false
}

func buildReactiveBid(ctx context.Context, view arb.View, o arb.Opportunity, poolFees []execution.ReactivePoolFee, reserve *big.Int, choice FundingChoice, conversion execution.FeeConversion, costs ReactiveCosts, cfg ReactivePolicy, base *big.Int, gasUnits, tipGas uint64, tip *big.Int, version domain.StateVersion, now time.Time) (execution.ReactiveIntent, error) {
	gas := new(big.Int).SetUint64(gasUnits)
	cap := new(big.Int).Add(base, tip)
	if cfg.MinimumFeeCap != nil && cap.Cmp(cfg.MinimumFeeCap) < 0 {
		cap.Set(cfg.MinimumFeeCap)
	}
	if cap.Cmp(cfg.MaxFee) > 0 {
		return execution.ReactiveIntent{}, fmt.Errorf("reactive fee cap exceeds policy")
	}
	budget := new(big.Int).Add(new(big.Int).Mul(cap, gas), costs.ExtraFor(len(o.Cycle.Pools), gasUnits))
	if cfg.MaxTotalFee == nil || budget.Cmp(cfg.MaxTotalFee) > 0 {
		return execution.ReactiveIntent{}, fmt.Errorf("reactive transaction budget exceeded")
	}
	if tipGas == gasUnits {
		tipGas = 0 // the whole limit; keeps the intent identity of a limit reserve
	}
	priced := execution.ReactiveIntent{Budget: budget, TipGas: tipGas, Call: execution.ExecutorCall{CallParameters: execution.CallParameters{GasLimit: gasUnits, FeeCap: cap, TipCap: tip}}}
	cost, err := execution.FeeInAsset(priced.GuardFee(), conversion)
	if err != nil {
		return execution.ReactiveIntent{}, err
	}
	expected, err := execution.CheckQuotedProfit(o.Net, reserve, choice.Asset.MaxLoanFee, cost, cfg.MinimumNet)
	if err != nil {
		return execution.ReactiveIntent{}, err
	}
	minimum := new(big.Int).Add(cost, cfg.MinimumNet)
	expires := min(now.Add(cfg.Lifetime).UnixMilli(), costs.ExpiresMS)
	route, err := Route(view, o, choice.Asset, Policy{TokenCheck: cfg.TokenCheck, PoolCheck: cfg.PoolCheck, Slippage: cfg.SlippageBPS, ValidSeconds: cfg.DeadlineSeconds}, minimum, uint64(now.Unix()))
	if err != nil {
		return execution.ReactiveIntent{}, err
	}
	output := new(big.Int).Add(o.Input, o.Net)
	var conditions *execution.TransactionConditions
	hot := cfg.HotPath && costs.ConfigVersion != 0 && hotSupported(cfg, route)
	if hot {
		candidate := route
		candidate.Steps = slices.Clone(route.Steps)
		if err := SetPriceBounds(view, &candidate, output); err != nil {
			return execution.ReactiveIntent{}, err
		}
		candidate.Hot, candidate.Chain = true, cfg.HotChain
		held := execution.HotConditions(cfg.Executor, costs.ConfigVersion, candidate.Deadline)
		holdQuietPools(view, &candidate, held, cfg.QuietPool, true)
		if hot = !cfg.HotChain || chainChecked(view, candidate); hot {
			route, conditions = candidate, held
		}
	}
	switch {
	case hot:
	case cfg.PricePrecheck:
		set := SetPriceBounds
		if cfg.CumulativePrecheck {
			set = SetPriceReferences
		}
		if err := set(view, &route, output); err != nil {
			return execution.ReactiveIntent{}, err
		}
	}
	if !route.Hot && cfg.PackedConditions {
		held := &execution.TransactionConditions{TimestampMax: route.Deadline}
		holdQuietPools(view, &route, held, cfg.QuietPool, false)
		if len(held.Accounts) > 0 {
			conditions = held
		}
	}
	call, err := execution.BuildExecutorCall(ctx, route, execution.CallParameters{Sender: cfg.Sender, Executor: cfg.Executor, CodeHash: cfg.CodeHash, GasLimit: gasUnits, FeeCap: cap, TipCap: tip})
	if err != nil {
		return execution.ReactiveIntent{}, err
	}
	return execution.SealReactiveIntent(ctx, execution.ReactiveIntent{Conditions: conditions, PoolFees: poolFees, QuotedGross: new(big.Int).Set(o.Net), QuoteReserve: reserve, Chain: cfg.Chain, Version: version, Route: route, Call: call, Budget: budget, TipGas: tipGas, ExpectedNet: expected, MinimumNet: new(big.Int).Set(cfg.MinimumNet), Conversion: conversion, CreatedMS: now.UnixMilli(), ExpiresMS: expires, ContextExpiresMS: costs.ExpiresMS})
}
