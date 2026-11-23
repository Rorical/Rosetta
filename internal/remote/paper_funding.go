package remote

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Selection and route assembly use the same cached funding rules. This only
// excludes known impossible funding; lender balances still require execution.
func paperFunding(ctx context.Context, opportunity arb.Opportunity, lenders []pool.State, wrapped domain.Address) (planning.FundingChoice, error) {
	var wallet *big.Int
	if opportunity.Cycle.Start == wrapped || opportunity.Cycle.Start == (domain.Address{}) {
		wallet = opportunity.Input
	}
	choices := planning.FundingChoices(opportunity, lenders, wallet, nil)
	if len(choices) == 0 {
		return planning.FundingChoice{}, fmt.Errorf("unsupported funding: no callback, independent lender or wrapped-native wallet")
	}
	return planning.ChooseFunding(ctx, opportunity, choices, big.NewInt(1), func(context.Context, planning.AssetPolicy) (*big.Int, error) {
		return new(big.Int), nil
	})
}
