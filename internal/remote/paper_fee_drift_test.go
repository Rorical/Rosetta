package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
)

func TestPaperFeeDiagnosisIncludesSuccessfulGrossDrift(t *testing.T) {
	for _, tc := range []struct {
		name                string
		quote, profit, loan int64
		delay               int64
		status              string
		want                bool
	}{
		{"observed successful mismatch", 591505702, 462375276, 0, 0, "success", true},
		{"underquote", 100, 120, 0, 0, "success", true},
		{"loan accounts for difference", 100, 80, 20, 0, "success", false},
		{"rounding", 100, 99, 0, 0, "success", false},
		{"below one percent of gross", 10000, 9990, 0, 0, "success", false},
		{"delayed state movement", 100, 50, 0, 200, "success", false},
		{"reverted baseline", 100, 0, 0, 0, "reverted", true},
		{"unavailable state", 100, 0, 0, 0, "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := paperExecutionJob{delayMS: tc.delay, opportunity: arb.Opportunity{Net: big.NewInt(tc.quote)}}
			result := paperExecutionResult{Status: tc.status, Profit: big.NewInt(tc.profit), LoanFee: big.NewInt(tc.loan)}
			if got := paperFeeDiagnosisNeeded(job, result); got != tc.want {
				t.Fatalf("diagnosis=%v want %v", got, tc.want)
			}
		})
	}
}
