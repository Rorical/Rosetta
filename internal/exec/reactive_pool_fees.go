package exec

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
)

// ReactivePoolFee binds a local Slipstream fee bound to this intent's signed
// priority cap. No account discount or positive base-fee threshold is assumed.
type ReactivePoolFee struct {
	Pool                 domain.PoolKey
	FeePips              uint32
	BaseFee, PriorityFee *big.Int
	Discount             uint32
}

func validateReactivePoolFees(intent ReactiveIntent) error {
	fees := make(map[domain.PoolKey]ReactivePoolFee, len(intent.PoolFees))
	for _, fee := range intent.PoolFees {
		if _, duplicate := fees[fee.Pool]; duplicate {
			return fmt.Errorf("duplicate reactive pool fee")
		}
		if fee.Pool.Kind != domain.PoolKindSlipstream || fee.FeePips > 100000 || fee.BaseFee == nil || fee.BaseFee.Sign() != 0 || fee.Discount != 0 || fee.PriorityFee == nil || intent.Call.TipCap == nil || fee.PriorityFee.Cmp(intent.Call.TipCap) != 0 {
			return fmt.Errorf("reactive pool fee does not cover signed priority cap")
		}
		fees[fee.Pool] = fee
	}
	for _, step := range intent.Route.Steps {
		switch step.Pool.Kind {
		case domain.PoolKindSlipstream:
			if _, ok := fees[step.Pool]; !ok {
				return fmt.Errorf("reactive Slipstream fee bound missing")
			}
			delete(fees, step.Pool)
		}
	}
	if len(fees) != 0 {
		return fmt.Errorf("reactive pool fee outside route")
	}
	return nil
}
