package planning

import (
	"math/big"
	"testing"
)

func TestForecastChangesCandidateCostAndRetainsBudgetQuotes(t *testing.T) {
	input := TipInput{Surplus: big.NewInt(10000), Gas: 100, RevertGas: 20, Base: big.NewInt(1), Extra: big.NewInt(0), Ceiling: big.NewInt(30), MinWin: .2,
		CandidateTips: []uint64{10, 20, 40}, Forecast: func(tip uint64, raw, baseline float64) (float64, uint64, bool) {
			if tip == 20 {
				return .8, 1000, true
			}
			if tip == 10 {
				return .7, 10, true
			}
			return .1, 10, true
		}}
	var quoted []uint64
	input.SurplusAtTip = func(tip uint64) (*big.Int, bool) { quoted = append(quoted, tip); return big.NewInt(9000), true }
	choice := ChooseTip(WinModel{}, input)
	if choice.Reason != "" || choice.Tip.Uint64() != 10 || choice.Win != .7 {
		t.Fatalf("wrong expected-cost choice: %+v", choice)
	}
	for _, tip := range quoted {
		if tip > 30 {
			t.Fatal("forecast escaped ceiling")
		}
	}
	input.Forecast = func(uint64, float64, float64) (float64, uint64, bool) { return .9, 10, false }
	if choice := ChooseTip(WinModel{}, input); choice.Reason != BidNoProfit {
		t.Fatalf("ineligible action sent: %+v", choice)
	}
}
