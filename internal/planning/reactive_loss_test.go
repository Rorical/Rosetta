package planning

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestLossGasFollowsHowTheRouteIsSent(t *testing.T) {
	p, q := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{4}}
	v := fundedView{p: {Key: p}, q: {Key: q}}
	o := arb.Opportunity{Cycle: graph.Cycle{Pools: []domain.PoolKey{p, q}, Len: 2}}
	held := map[domain.PoolKey]bool{}
	cfg := ReactivePolicy{HotPath: true, QuietPool: func(state pool.State) []execution.SlotCondition {
		if held[state.Key] {
			return []execution.SlotCondition{{}}
		}
		return nil
	}}
	costs := ReactiveCosts{ConfigVersion: 3, Poster: &PosterPrice{PerTx: new(big.Int), PerByte: new(big.Int)}}
	// The trigger is unheld: the race is lost at its bound.
	held[p] = true
	size := execution.HotCalldataBound(2)
	if gas, conditional := lossGas(cfg, v, o, execution.FundingV3FlashSwap, costs); gas != 21_000+16*uint64(size)+hotBoundGas || !conditional {
		t.Fatal("bounded loss", gas, conditional)
	}
	held[q] = true
	if gas, _ := lossGas(cfg, v, o, execution.FundingV3FlashSwap, costs); gas != FreeLoss {
		t.Fatal("a loss with every pool held is not free", gas)
	}
	for name, change := range map[string]func() (ReactivePolicy, execution.Funding, ReactiveCosts){
		"hot path off": func() (ReactivePolicy, execution.Funding, ReactiveCosts) {
			c := cfg
			c.HotPath = false
			return c, execution.FundingV3FlashSwap, costs
		},
		"no config version": func() (ReactivePolicy, execution.Funding, ReactiveCosts) {
			return cfg, execution.FundingV3FlashSwap, ReactiveCosts{}
		},
		"lender funding": func() (ReactivePolicy, execution.Funding, ReactiveCosts) { return cfg, execution.FundingV3Flash, costs },
	} {
		c, funding, k := change()
		if gas, conditional := lossGas(c, v, o, funding, k); gas != 0 || conditional {
			t.Error(name, "priced as a hot route", gas)
		}
	}
}

func TestPackedLossGasDependsOnHeldPools(t *testing.T) {
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{4}, PoolID: domain.Hash{1}}
	v := fundedView{p: {Key: p}, q: {Key: q}}
	o := arb.Opportunity{Cycle: graph.Cycle{Pools: []domain.PoolKey{p, q}, Len: 2}}
	held := map[domain.PoolKey]bool{}
	cfg := ReactivePolicy{PackedConditions: true, QuietPool: func(state pool.State) []execution.SlotCondition {
		if held[state.Key] {
			return []execution.SlotCondition{{}}
		}
		return nil
	}}
	if gas, conditional := lossGas(cfg, v, o, execution.FundingWallet, ReactiveCosts{}); gas != 0 || conditional {
		t.Fatal("a packed route with no held pool is conditional", gas)
	}
	held[q] = true
	if gas, conditional := lossGas(cfg, v, o, execution.FundingWallet, ReactiveCosts{}); gas != 0 || !conditional {
		t.Fatal("a partly held packed route lost its precheck prior", gas, conditional)
	}
	held[p] = true
	if gas, _ := lossGas(cfg, v, o, execution.FundingWallet, ReactiveCosts{}); gas != FreeLoss {
		t.Fatal("a fully held packed route is not free", gas)
	}
}
