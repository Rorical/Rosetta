package exec

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
)

// CheckQuotedProfit uses costs in the starting asset. Missing costs never
// become zero. The same arithmetic guards paper, confirmation and signing.
func CheckQuotedProfit(gross, reserve, loan, network, minimum *big.Int) (*big.Int, error) {
	if gross == nil || reserve == nil || loan == nil || network == nil || minimum == nil || reserve.Sign() < 0 || loan.Sign() < 0 || network.Sign() < 0 || minimum.Sign() <= 0 {
		return nil, fmt.Errorf("execution fee context unavailable")
	}
	net := new(big.Int).Sub(gross, reserve)
	net.Sub(net, loan).Sub(net, network)
	if net.Cmp(minimum) < 0 {
		return nil, fmt.Errorf("quoted profit does not cover funding, network fees and minimum net")
	}
	return net, nil
}

func CheckRouteTokens(route ExecutorPlan, check func(domain.Address) error) error {
	if check == nil {
		return nil
	}
	if err := check(route.Asset); err != nil {
		return err
	}
	for _, step := range route.Steps {
		if err := check(step.TokenIn); err != nil {
			return err
		}
	}
	return nil
}
