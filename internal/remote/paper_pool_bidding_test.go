package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
)

type bidPoolView map[domain.PoolKey]pool.State

func (v bidPoolView) Pool(k domain.PoolKey) (pool.State, bool) { p, ok := v[k]; return p, ok }
func (bidPoolView) Deep(domain.PoolKey) (pool.DeepState, bool) {
	return pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}, WindowMin: -2000, WindowMax: 2000}, true
}
func TestPaperBidScenarioChargesPoolTaxAndNetworkTip(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	first, second := domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	view := bidPoolView{
		first:  {Key: first, Token0: a, Token1: b, Fee: 100, TickSpacing: 10, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1e18), Slipstream: &pool.SlipstreamState{Valid: true, BlockFee: 100, SpacingFee: 100, TaxModule: domain.Address{5}, TaxMinimum: "100", TaxBaseFactor: "2", TaxMultiplier: 1_000_000_000_000}},
		second: {Key: second, Token0: a, Token1: b, Fee: 3000, Reserve0: big.NewInt(1_080_000_000_000_000_000), Reserve1: big.NewInt(1e18)},
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{first, second}, Len: 2}, Input: big.NewInt(1e12)}
	o, _, err := planning.QuoteTransaction(t.Context(), view, o, big.NewInt(1), new(big.Int), 0)
	if err != nil {
		t.Fatal(err)
	}
	p := &executionPaper{}
	job := paperExecutionJob{opportunity: o, event: arb.SearchEvent{Job: arb.SearchJob{View: view}}}
	result := &paperExecutionResult{Profit: new(big.Int).Set(o.Net), Call: &execution.ExecutorCall{CallParameters: execution.CallParameters{TipCap: new(big.Int)}}}
	budget := &paperBidBudget{FeeCapLimit: big.NewInt(200000), FeeUpperBound: big.NewInt(200000)}
	low := p.poolBidScenario(t.Context(), job, result, a, big.NewInt(1), budget, big.NewInt(100000), new(big.Int))
	high := p.poolBidScenario(t.Context(), job, result, a, big.NewInt(1), budget, big.NewInt(100000), big.NewInt(100000))
	if low.Error != "" || high.Error != "" || !low.Positive || high.Positive {
		t.Fatal(low, high)
	}
	networkOnly := new(big.Int).Sub(low.Net, big.NewInt(10000000000))
	if high.Net.Cmp(networkOnly) >= 0 || high.PoolGross.Cmp(o.Net) >= 0 {
		t.Fatal("pool tax missing from bid scenario", high)
	}
}
