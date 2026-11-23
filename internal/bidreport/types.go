// Package bidreport evaluates recorded bid predictions without fitting a model
// or accessing the network. Scores describe observed sends, not counterfactual
// wins at untried tips or opportunities the sending policy rejected.
package bidreport

import (
	"math/big"
	"time"
)

// Options bounds memory and selects sends by submission time, [Since, Until).
// Use deployment boundaries to avoid mixing different production policies.
type Options struct {
	Competition  []Competition
	Limit        int
	Since, Until time.Time
}

type Scores struct {
	Brier   float64 `json:"brier"`
	LogLoss float64 `json:"log_loss"`
	Mean    float64 `json:"mean_probability"`
}

type Group struct {
	Economics           Economics `json:"economics"`
	RevertGasSamples    int       `json:"revert_gas_samples"`
	RevertGasRatioMean  float64   `json:"revert_actual_over_predicted_gas_mean"`
	Name                string    `json:"name"`
	Count               int       `json:"count"`
	Success             int       `json:"success"`
	Observed            float64   `json:"observed_success_rate"`
	Raw                 Scores    `json:"raw"`
	Calibrated          Scores    `json:"calibrated"`
	Paid                int       `json:"paid_receipts"`
	SuccessFees         *big.Int  `json:"success_native_fees"`
	RevertFees          *big.Int  `json:"revert_native_fees"`
	SuccessGasSamples   int       `json:"success_gas_samples"`
	SuccessGasRatioMean float64   `json:"success_actual_over_predicted_gas_mean"`
}

type Report struct {
	Basis           string         `json:"basis"`
	Submitted       int            `json:"submitted_hashes"`
	Economics       Economics      `json:"economics_all_attempts"`
	FailureEvidence map[string]int `json:"failure_evidence_nonexclusive"`
	Age             []Group        `json:"receive_to_broadcast_age"`
	Model           []Group        `json:"model_versions"`
	Scored          int            `json:"scored"`
	Excluded        map[string]int `json:"excluded"`
	All             Group          `json:"all"`
	Probability     []Group        `json:"probability_bins"`
	Lane            []Group        `json:"lanes"`
	Hour            []Group        `json:"utc_hours"`
	Worth           []Group        `json:"worth_wei_per_gas_decades"`
}
