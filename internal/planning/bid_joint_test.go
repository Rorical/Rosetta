package planning

import (
	"math/big"
	"math/rand"
	"testing"
)

func TestJointTipSelectionMatchesIndependentExhaustiveCostOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(18))
	for range 100 {
		m := WinModel{Contenders: []Contender{{Presence: 1, Bids: []BidAtom{{Tip: 2, Weight: .4}, {Tip: 5, Weight: .3}, {Tip: 8, Weight: .3}}}}}
		tax := int64(rng.Intn(12))
		in := TipInput{Surplus: big.NewInt(150), Gas: 3, RevertGas: 2, Base: big.NewInt(1), Extra: big.NewInt(2), Ceiling: big.NewInt(10), MinWin: .1}
		in.SurplusAtTip = func(tip uint64) (*big.Int, bool) { return big.NewInt(150 - tax*int64(tip)), true }
		best, value := int64(0), -1e9
		for tip := int64(0); tip <= 10; tip++ {
			p := 0.0
			if tip > 2 {
				p += .4
			}
			if tip > 5 {
				p += .3
			}
			if tip > 8 {
				p += .3
			}
			ev := p*float64(150-tax*tip-3*tip) - (1-p)*float64(2*(1+tip)+2)
			if p >= .1 && ev > value {
				best, value = tip, ev
			}
		}
		got := ChooseTip(m, in)
		if value > 0 && (got.Reason != "" || got.Tip.Int64() != best) {
			t.Fatalf("tax %d chose %+v, oracle tip %d EV %v", tax, got, best, value)
		}
	}
}

func TestJointTipWorkIsBoundedAndRejectsInfeasibleCandidates(t *testing.T) {
	m := WinModel{Contenders: []Contender{{Presence: 1}}}
	for tip := uint64(1); tip < 60; tip++ {
		m.Contenders[0].Bids = append(m.Contenders[0].Bids, BidAtom{tip, 1.0 / 59})
	}
	calls := 0
	in := TipInput{Surplus: big.NewInt(10000), Gas: 1, RevertGas: 1, Base: new(big.Int), Extra: new(big.Int), Ceiling: big.NewInt(100), SurplusAtTip: func(uint64) (*big.Int, bool) { calls++; return nil, false }, MaxEvaluations: 7}
	got := ChooseTip(m, in)
	if calls != 7 || !got.Truncated || got.Reason != BidNoProfit || got.Expected.Sign() != 0 {
		t.Fatal(calls, got)
	}
}

func BenchmarkJointTipWith32PoolQuotes(b *testing.B) {
	view, o := transactionFixture()
	o, _, err := QuoteTransactionBound(b.Context(), view, o, new(big.Int))
	if err != nil {
		b.Fatal(err)
	}
	m := WinModel{Contenders: []Contender{{Presence: 1}}}
	for tip := uint64(1); tip <= 64; tip++ {
		m.Contenders[0].Bids = append(m.Contenders[0].Bids, BidAtom{tip * 1000, 1.0 / 64})
	}
	in := TipInput{Surplus: o.Net, Gas: 100000, RevertGas: 60000, Base: big.NewInt(1), Extra: new(big.Int), Ceiling: big.NewInt(100000), MaxEvaluations: 32}
	in.SurplusAtTip = func(tip uint64) (*big.Int, bool) {
		_, _, err := QuoteTransactionBound(b.Context(), view, o, new(big.Int).SetUint64(tip))
		if err != nil {
			b.Fatal(err)
		}
		return nil, false // Force all 32 quotes, including infeasible upper candidates.
	}
	b.ResetTimer()
	for range b.N {
		ChooseTip(m, in)
	}
}
