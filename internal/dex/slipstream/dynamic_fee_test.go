package slipstream

import (
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"math/big"
	"testing"
)

func dynamicPool() pool.State {
	return pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindSlipstream}, Tick: 20, Fee: 100,
		Slipstream: &pool.SlipstreamState{Valid: true, Block: 10, Timestamp: 100, BaseFee: 100, SpacingFee: 500, DefaultFeeCap: 50000, DefaultScalingFactor: 1_000_000, SecondsAgo: 10, ObservationIndex: 1, Cardinality: 2, CardinalityNext: 2, Complete: true,
			Observations: pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: 90, TickCumulative: 0}, {Index: 1, Timestamp: 100, TickCumulative: 100}})}}
}

func TestDynamicFeeTracksFirstSwapAndNextBlock(t *testing.T) {
	p := dynamicPool()
	if fee, err := LocalFee(p); err != nil || fee != 110 {
		t.Fatalf("before swap %d %v", fee, err)
	}
	old := p.Copy()
	FollowEvent(&p, uniswapv3.Event{Kind: uniswapv3.EventSwap, Tick: 40})
	p.Tick = 40
	RefreshFee(&p)
	if p.Fee != 110 || p.Slipstream.BlockFee != 110 {
		t.Fatalf("fee follows block cache: %+v", p.Slipstream)
	}
	AdvanceTime(&p, domain.BlockRef{Number: 11, Timestamp: 102})
	// 8 seconds at tick 10, then 2 seconds at tick 40: TWAP 16.
	if p.Fee != 124 || p.Slipstream.BlockFee != 0 {
		t.Fatalf("new block fee %d", p.Fee)
	}
	FollowEvent(&p, uniswapv3.Event{Kind: uniswapv3.EventMint, TickLower: 0, TickUpper: 50, Amount: big.NewInt(1)})
	if p.Slipstream.Observations.At(1).TickCumulative != 180 {
		t.Fatal(p.Slipstream.Observations)
	}
	if old.Slipstream.Observations.At(1).TickCumulative != 100 || old.Slipstream.BlockFee != 0 {
		t.Fatal("shared snapshot mutated")
	}
}

func TestDynamicFeeArithmeticAndFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*pool.State)
		want    uint32
		invalid bool
	}{
		{name: "negative TWAP rounds down", change: func(p *pool.State) {
			p.Tick = 0
			p.Slipstream.Observations = pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: 90}, {Index: 1, Timestamp: 100, TickCumulative: -1}})
		}, want: 101},
		{name: "large factor caps without overflow", change: func(p *pool.State) { p.Tick = 887272; p.Slipstream.DefaultScalingFactor = 1e18 }, want: 50000},
		{name: "base sentinel uses spacing fee", change: func(p *pool.State) { p.Slipstream.BaseFee = 0xffffff }, want: 500},
		{name: "initial fee bypasses cap", change: func(p *pool.State) {
			p.Slipstream.InitialFeeEnabled = true
			p.Slipstream.InitialFee = 800
			p.Slipstream.DefaultFeeCap = 50
		}, want: 800},
		{name: "zero scaling resets both defaults", change: func(p *pool.State) { p.Slipstream.FeeCap = 10 }, want: 110},
		{name: "actual short history has no dynamic component", change: func(p *pool.State) { p.Slipstream.SecondsAgo = 20 }, want: 100},
		{name: "missing local history cannot imply zero dynamic fee", change: func(p *pool.State) { p.Slipstream.SecondsAgo = 20; p.Slipstream.Complete = false }, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := dynamicPool()
			tt.change(&p)
			fee, err := LocalFee(p)
			if (err != nil) != tt.invalid || !tt.invalid && fee != tt.want {
				t.Fatalf("fee %d err %v want %d", fee, err, tt.want)
			}
		})
	}
}

func TestOracleWrapGrowthAndSameTimestamp(t *testing.T) {
	p := dynamicPool()
	m := p.Slipstream
	m.Timestamp = 2
	m.ObservationIndex = 1
	m.CardinalityNext = 4
	m.Observations = pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: ^uint32(0) - 9, TickCumulative: (1 << 55) - 20}, {Index: 1, Timestamp: ^uint32(0) - 1, TickCumulative: -(1 << 55) + 60}})
	c, ok := cumulativeAt(m, 2, 0, 20)
	if !ok || c != -(1<<55)+140 {
		t.Fatal(c, ok)
	}
	writeObservation(m, 20)
	if m.Cardinality != 4 || m.ObservationIndex != 2 {
		t.Fatal(m)
	}
	history := m.Observations
	writeObservation(m, 80)
	if m.Observations.Len() != history.Len() || m.Observations.At(m.Observations.Len()-1) != history.At(history.Len()-1) {
		t.Fatal("same timestamp wrote twice")
	}
}

func TestTransactionFeeDiscountAndPriorityTax(t *testing.T) {
	p := dynamicPool()
	m := p.Slipstream
	m.BlockFee = 111
	m.TaxModule = domain.Address{2}
	m.TaxMinimum = "100"
	m.TaxBaseFactor = "2"
	m.TaxMultiplier = 1_000_000_000_000
	fee, err := TransactionFee(p, big.NewInt(100), big.NewInt(203), 500000)
	if err != nil || fee != 59 {
		t.Fatalf("ceil(111/2)+3 = 59, got %d %v", fee, err)
	}
	fee, err = TransactionFee(p, big.NewInt(100), big.NewInt(200), 0)
	if err != nil || fee != 111 {
		t.Fatal(fee, err)
	}
	m.InitialFeeEnabled = true
	m.InitialFee = 100
	m.BlockFee = 0
	fee, err = TransactionFee(p, big.NewInt(100), big.NewInt(0), 500000)
	if err != nil || fee != 100 {
		t.Fatalf("initial fee must not be discounted: %d %v", fee, err)
	}
}

func TestLegacyFeeUsesLiveTickTruncationAndZeroSentinel(t *testing.T) {
	p := dynamicPool()
	m := p.Slipstream
	m.Legacy = true
	m.SecondsAgo = 2
	m.Observations = pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: 98}, {Index: 1, Timestamp: 100, TickCumulative: -1}})
	p.Tick = 0
	fee, err := LocalFee(p)
	if err != nil || fee != 100 {
		t.Fatal("legacy signed division must truncate", fee, err)
	}
	FollowEvent(&p, uniswapv3.Event{Kind: uniswapv3.EventSwap, Tick: 20})
	p.Tick = 20
	RefreshFee(&p)
	if p.Fee != 120 || m.BlockFee != 0 {
		t.Fatal("legacy must not cache the first swap fee", p.Fee, m.BlockFee)
	}
	m.BaseFee = 420
	fee, err = LocalFee(p)
	if err != nil || fee != 0 {
		t.Fatal("legacy explicit zero fee", fee, err)
	}
	m.BaseFee = 101
	m.DefaultScalingFactor = 0
	fee, err = TransactionFee(p, big.NewInt(100), big.NewInt(1000000), 500000)
	if err != nil || fee != 50 {
		t.Fatal("legacy discount subtracts ceil, no hook tax", fee, err)
	}
}

func TestLegacyCardinalityThresholdDoesNotApplyToHook(t *testing.T) {
	p := dynamicPool()
	p.Slipstream.Legacy = true
	fee, err := LocalFee(p)
	if err != nil || fee != 100 {
		t.Fatal(fee, err)
	}
	p.Slipstream.Legacy = false
	fee, err = LocalFee(p)
	if err != nil || fee != 110 {
		t.Fatal(fee, err)
	}
}

func BenchmarkLocalDynamicFee(b *testing.B) {
	p := dynamicPool()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := LocalFee(p); err != nil {
			b.Fatal(err)
		}
	}
}

// Vectors follow PriorityFeeMevTaxModule's mulDivUp(..., 1e12) and
// SwapHookLib's 100000-pip ceiling, including equality at the tax threshold.
func TestTransactionTaxThresholdRoundingAndTotalCeiling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, tip  int64
		multiplier uint64
		want       uint32
	}{
		{"minimum threshold", 10, 100, 1, 111},
		{"base threshold", 100, 200, 1, 111},
		{"one wei above threshold rounds up", 100, 201, 1, 112},
		{"fraction rounds up", 100, 203, 500000000000, 113},
		{"combined ceiling", 0, 200000, 1000000000000, 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := dynamicPool()
			p.Slipstream.BlockFee = 111
			p.Slipstream.TaxModule = domain.Address{2}
			p.Slipstream.TaxMinimum, p.Slipstream.TaxBaseFactor, p.Slipstream.TaxMultiplier = "100", "2", tc.multiplier
			got, err := TransactionFee(p, big.NewInt(tc.base), big.NewInt(tc.tip), 0)
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}

// Robinhood's Aero Slipstream factory has no local fee model here, so its
// fee is read each block. Of 532 in-tick swaps on its WETH/USDG pool,
// every charged fee but one equalled fee() of the swap's block.
// With no tip and no discount, no tip-dependent tax applies and fee() is
// the transaction's fee; otherwise the module is unknown and refused.
func TestTransactionFeeWithoutALocalModel(t *testing.T) {
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{1}}, Fee: 70}
	if fee, err := TransactionFee(p, big.NewInt(20_000_000), new(big.Int), 0); err != nil || fee != 70 {
		t.Fatalf("zero tip: fee %d, %v", fee, err)
	}
	if _, err := TransactionFee(p, big.NewInt(20_000_000), big.NewInt(1), 0); err == nil {
		t.Fatal("a tip on an unmodelled pool was priced")
	}
	if _, err := TransactionFee(p, big.NewInt(20_000_000), new(big.Int), 1); err == nil {
		t.Fatal("a discount on an unmodelled pool was priced")
	}
}
