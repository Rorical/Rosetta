package planning

import (
	"context"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// BidRequest binds competition pricing to one immutable quote and funding choice.
// Surplus is after reserved base costs and minimum retained profit. Repricing
// never performs RPC, mutates the view, or changes the principal being compared.
type BidRequest struct {
	Route                            []domain.PoolKey
	Funding                          execution.Funding
	Amount                           *big.Int
	Surplus                          *big.Int
	Gas                              uint64
	Base, Extra, Ceiling, MinimumTip *big.Int
	ReceivedAt                       time.Time
	Lane                             string
	SurplusAtTip                     func(uint64) (*big.Int, bool)
	// Reserved is the native base-fee budget Surplus and SurplusAtTip already
	// exclude: the fee cap's base share on TipGas. Nil means the share-of-
	// limit reserve of OP's original policy, twice the base fee on Gas.
	Reserved *big.Int
	// LossGas is what losing costs this route when it is known from how it
	// will be sent (lossGas), zero otherwise. Conditional routes learn their
	// loss cost apart from plain ones.
	LossGas     uint64
	Conditional bool
	// ShareTip, when set, is the least tip to bid: the route's share of its
	// value (valueShareTip), already within Ceiling.
	ShareTip *big.Int
}

type pricedReactiveQuote struct {
	opportunity arb.Opportunity
	fees        []execution.ReactivePoolFee
	reserve     *big.Int
}

func quoteReserve(o arb.Opportunity, bps uint64) *big.Int {
	reserve := new(big.Int).Add(o.Input, o.Net)
	reserve.Mul(reserve, new(big.Int).SetUint64(bps))
	return reserve.Add(reserve, big.NewInt(9999)).Quo(reserve, big.NewInt(10000))
}

// transactionBidCosts caches only bounded, explicitly evaluated candidates.
// The final build reuses the exact quote that contributed to its expected value.
func transactionBidCosts(ctx context.Context, view arb.View, o arb.Opportunity, choice FundingChoice, conversion execution.FeeConversion, cfg ReactivePolicy, base, extra *big.Int, gas, tipGas uint64) (func(uint64) (*big.Int, bool), map[uint64]pricedReactiveQuote) {
	quotes := make(map[uint64]pricedReactiveQuote)
	return func(tip uint64) (*big.Int, bool) {
		quoted, fees, err := QuoteTransactionBound(ctx, view, o, new(big.Int).SetUint64(tip))
		if err != nil {
			return nil, false
		}
		reserve := quoteReserve(quoted, cfg.QuoteReserveBPS)
		// The profit guard charges the fee cap on tipGas (GuardFee), as the
		// zero-tip surplus does; BidRequest.Reserved names this share.
		budget := new(big.Int).Mul(base, new(big.Int).SetUint64(tipGas))
		budget.Add(budget, extra)
		cost, err := execution.FeeInAsset(budget, conversion)
		if err != nil {
			return nil, false
		}
		net := new(big.Int).Sub(quoted.Net, reserve)
		net.Sub(net, choice.Asset.MaxLoanFee).Sub(net, cost).Sub(net, cfg.MinimumNet)
		native := net.Mul(net, conversion.Denominator).Quo(net, conversion.Numerator)
		priorityBudget := new(big.Int).Mul(new(big.Int).SetUint64(tip), new(big.Int).SetUint64(tipGas))
		if native.Cmp(priorityBudget) < 0 {
			return nil, false
		}
		quotes[tip] = pricedReactiveQuote{quoted, fees, reserve}
		return native, true
	}, quotes
}
