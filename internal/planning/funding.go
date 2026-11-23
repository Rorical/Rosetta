package planning

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

var ErrFundingUnprofitable = errors.New("gross profit does not cover funding and execution budget")

// ErrNetworkUnprofitable is the ErrFundingUnprofitable case where the gross
// does not cover the execution budget even without a loan fee. A smaller
// principal, with a lower gross and the same budget, cannot recover it.
var ErrNetworkUnprofitable = fmt.Errorf("%w: even without a loan fee", ErrFundingUnprofitable)

// FundingShortfall is a funding rejection with the gross it judged and the
// cheapest execution budget it held the gross against, both in the starting
// asset. Skip statistics bucket the ratio to show which costs block a route.
type FundingShortfall struct {
	Err           error
	Gross, Budget *big.Int
}

func (e *FundingShortfall) Error() string { return e.Err.Error() }
func (e *FundingShortfall) Unwrap() error { return e.Err }

// FundingChoice binds principal and costs to one exact quote. NetworkCost is
// denominated in the starting asset and must include the chosen bid.
type FundingChoice struct {
	Asset       AssetPolicy
	NetworkCost *big.Int
	Net         *big.Int
}

// LendsInSwap reports whether a pool of this kind pays a swap's output before
// its callback collects the input, so a route starting on it needs no loan.
func LendsInSwap(kind domain.PoolKind) bool {
	return kind == domain.PoolKindV3 || kind == domain.PoolKindSlipstream || kind == domain.PoolKindSolidlyV3
}

// FundingChoices uses only cached state. A wallet entry is the lesser of its
// available balance and allowance; inventory is the executor's own balance of
// the start asset, nil when the asset is not funded from it. Lender balances must be supplied separately
// by the caller before a choice can be admitted for live execution.
func FundingChoices(o arb.Opportunity, lenders []pool.State, wallet, inventory *big.Int) []AssetPolicy {
	if o.Input == nil || o.Input.Sign() <= 0 || len(o.Cycle.Pools) < 2 {
		return nil
	}
	makeAsset := func(f execution.Funding) AssetPolicy {
		return AssetPolicy{Funding: f, MaxLoanFee: new(big.Int), MinNetProfit: big.NewInt(1)}
	}
	var choices []AssetPolicy
	if inventory != nil && inventory.Cmp(o.Input) >= 0 && o.Cycle.Start != (domain.Address{}) {
		choices = append(choices, makeAsset(execution.FundingInventory))
	}
	if wallet != nil && wallet.Cmp(o.Input) >= 0 {
		choices = append(choices, makeAsset(execution.FundingWallet))
	}
	if o.Cycle.Start != (domain.Address{}) && LendsInSwap(o.Cycle.Pools[0].Kind) {
		choices = append(choices, makeAsset(execution.FundingV3FlashSwap))
	}
	allV4 := true
	for _, key := range o.Cycle.Pools {
		allV4 = allV4 && key.IsV4() && key.Address == o.Cycle.Pools[0].Address
	}
	if allV4 {
		choices = append(choices, makeAsset(execution.FundingV4Flash))
	}
	for _, lender := range lenders {
		if lender.Key.Kind != domain.PoolKindV3 || lender.Fee >= 1_000_000 || o.Cycle.Start == (domain.Address{}) || (lender.Token0 != o.Cycle.Start && lender.Token1 != o.Cycle.Start) {
			continue
		}
		locked := false
		for _, key := range o.Cycle.Pools {
			locked = locked || key.Address == lender.Key.Address
		}
		if locked {
			continue
		}
		asset := makeAsset(execution.FundingV3Flash)
		asset.Lender = lender.Key
		asset.MaxLoanFee.Mul(o.Input, new(big.Int).SetUint64(uint64(lender.Fee)))
		asset.MaxLoanFee.Add(asset.MaxLoanFee, big.NewInt(999999)).Quo(asset.MaxLoanFee, big.NewInt(1000000))
		choices = append(choices, asset)
	}
	return choices
}

// ChooseFunding compares total costs, not lender order or raw gross profit.
// A nil cost means unavailable evidence, never free execution. Equal costs
// prefer inventory and callback funding, then wallet, then a deterministically chosen lender.
func ChooseFunding(ctx context.Context, o arb.Opportunity, choices []AssetPolicy, minimum *big.Int,
	cost func(context.Context, AssetPolicy) (*big.Int, error)) (FundingChoice, error) {
	if o.Net == nil || minimum == nil || minimum.Sign() <= 0 {
		return FundingChoice{}, fmt.Errorf("invalid funding profit bounds")
	}
	var best FundingChoice
	var cheapest *big.Int
	coversNetwork := false
	for _, asset := range choices {
		if err := ctx.Err(); err != nil {
			return FundingChoice{}, err
		}
		network, err := cost(ctx, asset)
		if err != nil {
			return FundingChoice{}, err
		}
		if network == nil {
			continue
		}
		if network.Sign() < 0 {
			return FundingChoice{}, fmt.Errorf("negative execution budget")
		}
		if cheapest == nil || network.Cmp(cheapest) < 0 {
			cheapest = network
		}
		if new(big.Int).Sub(o.Net, network).Cmp(minimum) >= 0 {
			coversNetwork = true
		}
		net := new(big.Int).Sub(o.Net, asset.MaxLoanFee)
		net.Sub(net, network)
		if net.Cmp(minimum) < 0 {
			continue
		}
		if best.Net == nil || net.Cmp(best.Net) > 0 || net.Cmp(best.Net) == 0 && fundingBefore(asset, best.Asset) {
			asset.MinNetProfit = new(big.Int).Set(minimum)
			best = FundingChoice{asset, new(big.Int).Set(network), net}
		}
	}
	if best.Net == nil {
		err := ErrFundingUnprofitable
		if !coversNetwork {
			err = ErrNetworkUnprofitable
		}
		if cheapest == nil {
			return best, err
		}
		return best, &FundingShortfall{Err: err, Gross: new(big.Int).Set(o.Net), Budget: new(big.Int).Set(cheapest)}
	}
	return best, nil
}

func fundingBefore(a, b AssetPolicy) bool {
	rank := func(f execution.Funding) int {
		if f == execution.FundingV3FlashSwap || f == execution.FundingV4Flash || f == execution.FundingInventory {
			return 0
		}
		if f == execution.FundingWallet {
			return 1
		}
		return 2
	}
	if rank(a.Funding) != rank(b.Funding) {
		return rank(a.Funding) < rank(b.Funding)
	}
	return bytes.Compare(a.Lender.Address[:], b.Lender.Address[:]) < 0
}
