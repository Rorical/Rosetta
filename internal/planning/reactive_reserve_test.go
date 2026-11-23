package planning

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"math/big"
	"testing"
	"time"
)

func reactiveReserveFixture() (fundedView, arb.Opportunity, ReactiveCosts, ReactivePolicy, domain.StateVersion, time.Time) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	view := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	amount, gross := big.NewInt(1_000_000_000), big.NewInt(1_000_001)
	out := new(big.Int).Add(amount, gross)
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: amount, Output: out, Net: gross, Legs: []arb.Leg{{Pool: p, AmountIn: amount, AmountOut: amount}, {Pool: q, AmountIn: amount, AmountOut: out}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(1), Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 21000, MaxFee: big.NewInt(1000000), MaxTip: new(big.Int), TargetTip: new(big.Int), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}
	return view, o, costs, cfg, version, now
}
func TestReactiveQuoteReserveCoversSwapOutputNotOnlyProfit(t *testing.T) {
	view, o, costs, cfg, version, now := reactiveReserveFixture()
	cfg.QuoteReserveBPS = 20
	_, err := BuildReactive(t.Context(), view, o, nil, o.Input, version, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err == nil {
		t.Fatal("thin profit admitted with a reserve computed only on profit")
	}
	cfg.QuoteReserveBPS = 1
	intent, err := BuildReactive(t.Context(), view, o, nil, o.Input, version, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if intent.QuoteReserve.Cmp(big.NewInt(100101)) != 0 {
		t.Fatalf("reserve=%s want=100101", intent.QuoteReserve)
	}
}

// Slipstream's fee depends on the signed tip, so it needs the recomputed bound.
// Other snapshot fees are admitted: a wrong fee fails the executor's profit and
// minimum-output checks, costing gas but never principal.
func TestReactiveFeeBoundOnlyForTipDependentFees(t *testing.T) {
	for _, chain := range []domain.ChainIdentity{{Family: domain.FamilyOPStack, ChainID: 10}, {Family: domain.FamilyNitro, ChainID: 42161}} {
		for _, kind := range []domain.PoolKind{domain.PoolKindSlipstream, domain.PoolKindCamelotV3, domain.PoolKindVelodromeV2, domain.PoolKindV4} {
			view, o, costs, cfg, version, now := reactiveReserveFixture()
			cfg.Chain = chain
			prior := o.Cycle.Pools[0]
			key := prior
			key.Kind = kind
			if kind == domain.PoolKindV4 {
				key.PoolID = domain.Hash{1}
			}
			p := view[prior]
			p.Key = key
			view[key] = p
			delete(view, prior)
			o.Cycle.Pools[0] = key
			o.Legs[0].Pool = key
			_, err := BuildReactive(t.Context(), view, o, nil, o.Input, version, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
			if kind == domain.PoolKindSlipstream && err == nil {
				t.Fatalf("Slipstream admitted without a tip-bound fee for chain=%d", chain.ChainID)
			}
			if kind != domain.PoolKindSlipstream && err != nil {
				t.Fatalf("snapshot fee rejected for chain=%d kind=%d: %v", chain.ChainID, kind, err)
			}
		}
	}
}
