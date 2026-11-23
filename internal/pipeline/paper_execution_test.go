package pipeline

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"math/big"
	"testing"
)

func TestExecutionEconomicsDoesNotChargeLoanTwiceOrRequireReceiptSamples(t *testing.T) {
	var research *Research
	wrapped := domain.Address{1}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: wrapped}}
	result := research.ExecutionEconomics(nil, o, wrapped, big.NewInt(100), big.NewInt(5), big.NewInt(30))
	if result.Gross.Int64() != 105 || result.Net.Int64() != 70 || result.Status != NetEligible {
		t.Fatalf("simulation economics %+v", result)
	}
	result = research.ExecutionEconomics(nil, o, wrapped, big.NewInt(100), big.NewInt(5), big.NewInt(130))
	if result.Net.Int64() != -30 || result.Status != NetNonPositive {
		t.Fatalf("loss misclassified %+v", result)
	}
	result = research.ExecutionEconomics(nil, o, wrapped, big.NewInt(100), big.NewInt(5), nil)
	if result.Status != NetUnknown || result.Net != nil {
		t.Fatal("unknown fee became a zero cost")
	}
}
