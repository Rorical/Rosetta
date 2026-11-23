package planning

import (
	"context"
	"errors"
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"math/big"
	"testing"
)

func TestFundingChoosesCostInsteadOfLenderOrder(t *testing.T) {
	token := domain.Address{1}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: token, Pools: []domain.PoolKey{{Kind: domain.PoolKindV2, Address: domain.Address{2}}, {Kind: domain.PoolKindV2, Address: domain.Address{3}}}}, Input: big.NewInt(1000000), Net: big.NewInt(700)}
	lenders := []pool.State{{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{4}}, Token0: token, Fee: 500}, {Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{5}}, Token0: token, Fee: 100}, {Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{2}}, Token0: token, Fee: 0}}
	choices := FundingChoices(o, lenders, o.Input, nil)
	best, err := ChooseFunding(t.Context(), o, choices, big.NewInt(1), func(_ context.Context, a AssetPolicy) (*big.Int, error) {
		if a.Funding == execution.FundingWallet {
			return big.NewInt(400), nil
		}
		return big.NewInt(50), nil
	})
	if err != nil || best.Asset.Lender != lenders[1].Key || best.Net.Int64() != 550 {
		t.Fatal(best, err)
	}
	o.Net = big.NewInt(50)
	if _, err = ChooseFunding(t.Context(), o, choices, big.NewInt(1), func(context.Context, AssetPolicy) (*big.Int, error) { return big.NewInt(50), nil }); !errors.Is(err, ErrFundingUnprofitable) {
		t.Fatal(err)
	}
}

func TestFundingRejectsObservedLoanLossAndUnknownFees(t *testing.T) {
	o := arb.Opportunity{Net: big.NewInt(13684)}
	a := AssetPolicy{Funding: execution.FundingV3Flash, MaxLoanFee: big.NewInt(49040)}
	for _, cost := range []*big.Int{nil, new(big.Int)} {
		_, err := ChooseFunding(t.Context(), o, []AssetPolicy{a}, big.NewInt(1), func(context.Context, AssetPolicy) (*big.Int, error) { return cost, nil })
		if !errors.Is(err, ErrFundingUnprofitable) {
			t.Fatal(err)
		}
	}
}

func TestSlipstreamUsesCallbackFundingBeforeStandaloneLoan(t *testing.T) {
	o := arb.Opportunity{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{{Kind: domain.PoolKindSlipstream, Address: domain.Address{2}}, {Kind: domain.PoolKindV2, Address: domain.Address{3}}}}, Input: big.NewInt(100), Net: big.NewInt(3)}
	choices := FundingChoices(o, nil, nil, nil)
	choice, err := ChooseFunding(t.Context(), o, choices, big.NewInt(1), func(context.Context, AssetPolicy) (*big.Int, error) { return new(big.Int), nil })
	if err != nil || choice.Asset.Funding != execution.FundingV3FlashSwap || choice.Asset.MaxLoanFee.Sign() != 0 {
		t.Fatal(choice, err)
	}
}

// A gross that does not cover the execution budget even without a loan fee
// cannot recover at a smaller principal, whose gross is lower and budget the
// same; one blocked only by a proportional loan fee can.
func TestFundingTellsALoanFeeShortfallFromANetworkOne(t *testing.T) {
	token := domain.Address{1}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: token, Pools: []domain.PoolKey{{Kind: domain.PoolKindV2, Address: domain.Address{2}}, {Kind: domain.PoolKindV2, Address: domain.Address{3}}}}, Input: big.NewInt(1000000), Net: big.NewInt(100)}
	loan := []AssetPolicy{{Funding: execution.FundingV3Flash, MaxLoanFee: big.NewInt(80)}}
	cost := func(context.Context, AssetPolicy) (*big.Int, error) { return big.NewInt(50), nil }
	if _, err := ChooseFunding(t.Context(), o, loan, big.NewInt(1), cost); !errors.Is(err, ErrFundingUnprofitable) || errors.Is(err, ErrNetworkUnprofitable) {
		t.Fatalf("loan-fee shortfall: %v", err)
	}
	o.Net = big.NewInt(40)
	_, err := ChooseFunding(t.Context(), o, loan, big.NewInt(1), cost)
	if !errors.Is(err, ErrNetworkUnprofitable) || !errors.Is(err, ErrFundingUnprofitable) {
		t.Fatalf("network shortfall: %v", err)
	}
	var short *FundingShortfall
	if !errors.As(err, &short) || short.Gross.Int64() != 40 || short.Budget.Int64() != 50 {
		t.Fatalf("shortfall amounts: %v", short)
	}
}

// Inventory is the executor's own balance: it funds a route whose first pool
// cannot lend, when it covers the principal, and is preferred at equal cost.
func TestInventoryFundsARouteItCovers(t *testing.T) {
	token := domain.Address{1}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: token, Pools: []domain.PoolKey{{Kind: domain.PoolKindMetricOmm, Address: domain.Address{2}}, {Kind: domain.PoolKindV3, Address: domain.Address{3}}}}, Input: big.NewInt(1000), Net: big.NewInt(50)}
	lenders := []pool.State{{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{4}}, Token0: token, Fee: 100}}
	free := func(context.Context, AssetPolicy) (*big.Int, error) { return big.NewInt(10), nil }
	choice, err := ChooseFunding(t.Context(), o, FundingChoices(o, lenders, nil, big.NewInt(1000)), big.NewInt(1), free)
	if err != nil || choice.Asset.Funding != execution.FundingInventory || choice.Asset.MaxLoanFee.Sign() != 0 || choice.Net.Int64() != 40 {
		t.Fatal(choice, err)
	}
	for name, inventory := range map[string]*big.Int{"short": big.NewInt(999), "not funded": nil} {
		choice, err = ChooseFunding(t.Context(), o, FundingChoices(o, lenders, nil, inventory), big.NewInt(1), free)
		if err != nil || choice.Asset.Funding != execution.FundingV3Flash {
			t.Fatal(name, choice, err)
		}
	}
	o.Cycle.Start = domain.Address{}
	if choices := FundingChoices(o, nil, nil, big.NewInt(1000)); len(choices) != 0 {
		t.Fatal("native start funded from inventory", choices)
	}
}
