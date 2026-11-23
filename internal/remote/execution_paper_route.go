package remote

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
)

func paperRoute(ctx context.Context, view arb.View, opportunity arb.Opportunity, lenders []pool.State, wrapped domain.Address, slippage, timestamp uint64) (execution.ExecutorPlan, error) {
	distinct := map[domain.PoolKey]bool{}
	for _, key := range opportunity.Cycle.Pools {
		if err := execution.CheckExecutorPool(key); err != nil {
			return execution.ExecutorPlan{}, fmt.Errorf("unsupported executor route: %w", err)
		}
		if _, ok := view.Pool(key); !ok {
			return execution.ExecutorPlan{}, fmt.Errorf("missing route pool")
		}
		distinct[key] = true
	}
	if len(distinct) < 2 {
		return execution.ExecutorPlan{}, fmt.Errorf("execution paper needs a multi-pool cycle")
	}
	// Paper rejects provably losing funding before spending trace requests.
	// Network fees are measured by the execution, not assumed to be known here.
	choice, err := paperFunding(ctx, opportunity, lenders, wrapped)
	if err != nil {
		return execution.ExecutorPlan{}, err
	}
	plan, err := planning.Route(view, opportunity, choice.Asset, planning.Policy{Slippage: slippage, ValidSeconds: 10}, big.NewInt(1), timestamp)
	if err == nil {
		_, err = execution.EncodePackedExecutorPlan(ctx, plan)
	}
	return plan, err
}
