package remote

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// The exact key separates routes by funding, pool and order; the shape key
// only by funding and pool kinds.
func TestRouteGasKeysSeparateRoutes(t *testing.T) {
	a := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0x0a, 0xbc}}
	b := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xff}, PoolID: domain.Hash{0x01, 0x02}}
	c := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xff}, PoolID: domain.Hash{0x01, 0x03}}
	exact, shape := routeGasKeys([]domain.PoolKey{a, b}, execution.FundingWallet)
	if exact != "0|1:0abc000000000000000000000000000000000000:0000000000000000000000000000000000000000000000000000000000000000|2:ff00000000000000000000000000000000000000:0102000000000000000000000000000000000000000000000000000000000000" || shape != "0|1|2" {
		t.Fatalf("keys %q %q", exact, shape)
	}
	for name, other := range map[string]struct {
		route   []domain.PoolKey
		funding execution.Funding
	}{
		"other pool id": {[]domain.PoolKey{a, c}, execution.FundingWallet},
		"other order":   {[]domain.PoolKey{b, a}, execution.FundingWallet},
		"other funding": {[]domain.PoolKey{a, b}, execution.FundingV3Flash},
	} {
		if got, _ := routeGasKeys(other.route, other.funding); got == exact {
			t.Fatalf("%s shares the exact key", name)
		}
	}
	if _, got := routeGasKeys([]domain.PoolKey{a, c}, execution.FundingWallet); got != shape {
		t.Fatalf("same kinds, shape %q", got)
	}
}
