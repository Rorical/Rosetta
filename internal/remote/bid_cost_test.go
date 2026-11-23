package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// OP receipts typically include 38,940/47,772-gas PoolMoved
// reverts and a 415,292-gas successful route. A losing streak must not
// make the model price a complete execution as another early revert.
func TestBidSuccessGasExcludesEarlyReverts(t *testing.T) {
	r := &reactiveRuntime{bids: newBidEstimator(time.Second, 2*time.Second)}
	book := func(success bool, used uint64) {
		intent := &execution.ReactiveIntent{}
		intent.Call.GasLimit = 1_050_000
		r.observeSettlement(execution.SettlementUpdate{FeesBooked: true, Settlement: &execution.Settlement{
			Success: success,
			Intent:  intent,
			Fees:    &domain.ReceiptFees{GasUsed: used, Total: big.NewInt(1)},
		}})
	}
	for range gasMinRatios {
		book(true, 415_292)
	}
	before := r.bids.expectedGas(1_050_000)
	for i := range gasRatios {
		book(false, []uint64{38_940, 47_772}[i%2])
	}
	if got := r.bids.expectedGas(1_050_000); got != before || got < 415_292 {
		t.Fatalf("early reverts changed successful gas from %d to %d", before, got)
	}
}

func TestBidGasMigrationDropsUnclassifiedHistory(t *testing.T) {
	e := newBidEstimator(time.Second, 2*time.Second)
	// Format 3 cannot tell whether its ratios came from successful swaps.
	raw := []byte(`{"format":3,"gas":[0.04,0.05,0.04,0.05,0.04],"calibration":[{"t":1799999999000,"p":0.5,"w":100000,"y":true}]}`)
	if err := e.decode(raw, time.UnixMilli(1_800_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if got := e.expectedGas(1_000_000); got != 600_000 || len(e.calibration.points) != 1 {
		t.Fatalf("migration gas=%d, retained calibration=%d", got, len(e.calibration.points))
	}
}

// A chain's own prior stands in until its receipts settle, then gives way.
func TestExpectedGasUsesTheChainPriorUntilReceiptsSettle(t *testing.T) {
	e := newBidEstimator(time.Second, 2*time.Second)
	e.gasPrior = 0.5
	if got := e.expectedGas(1_000_000); got != 500_000 {
		t.Fatalf("prior gas=%d", got)
	}
	for range gasMinRatios {
		e.observeGas(300_000, 1_000_000)
	}
	if got := e.expectedGas(1_000_000); got != 300_000 {
		t.Fatalf("settled gas=%d", got)
	}
}
