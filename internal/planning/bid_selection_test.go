package planning

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// Exhaust every affordable integer tip independently of the optimizer's
// candidate grid. This covers ties, sparse bid atoms and capped floors.
func TestChooseTipMatchesExhaustiveSmallAuctions(t *testing.T) {
	rng := rand.New(rand.NewSource(20261103))
	for trial := range 300 {
		model := WinModel{First: rng.Float64() / 3, Shift: rng.Float64()*2 - 1}
		for range 1 + rng.Intn(5) {
			c := Contender{Presence: rng.Float64()}
			for range 4 {
				c.Bids = append(c.Bids, BidAtom{Tip: uint64(rng.Intn(60)), Weight: 0.25})
			}
			model.Contenders = append(model.Contenders, c)
		}
		in := TipInput{Surplus: big.NewInt(10_000), Gas: 100, Base: big.NewInt(2), Extra: big.NewInt(3), Ceiling: big.NewInt(50), MinWin: 0.2, RevertGas: 20, Floor: uint64(rng.Intn(60))}
		got := ChooseTip(model, in)
		best, likely := math.Inf(-1), math.Inf(-1)
		var bestTip, likelyTip uint64
		for tip := min(in.Floor, uint64(50)); tip <= 50; tip++ {
			raw := 1 - model.First
			for _, c := range model.Contenders {
				var tail float64
				for _, b := range c.Bids {
					if b.Tip >= tip {
						tail += b.Weight
					}
				}
				raw *= 1 - c.Presence*tail
			}
			p := Calibrate(raw, model.Shift)
			value := p*(10000-float64(tip)*100) - (1-p)*(20*(2+float64(tip))+3)
			if value > best {
				best, bestTip = value, tip
			}
			if p >= in.MinWin && value > likely {
				likely, likelyTip = value, tip
			}
		}
		want := bestTip
		if likely > 0 {
			want = likelyTip
		}
		if got.Tip.Uint64() != want {
			t.Fatalf("trial %d: selected %s, exhaustive %d", trial, got.Tip, want)
		}
	}
}

func BenchmarkChooseTipBoundedRivals(b *testing.B) {
	model := WinModel{First: 0.1, Shift: -0.3}
	for i := range 24 {
		c := Contender{Presence: 0.1}
		for j := range 48 {
			c.Bids = append(c.Bids, BidAtom{Tip: uint64(1000*j + i), Weight: 1.0 / 48})
		}
		model.Contenders = append(model.Contenders, c)
	}
	in := tipInput(4_000_000_000_000, 1_000_000, 0.2)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ChooseTip(model, in)
	}
}
