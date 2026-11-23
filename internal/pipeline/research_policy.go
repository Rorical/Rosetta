package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/Rorical/Rosetta/internal/domain"
)

type ResearchPolicy struct {
	Paper        *PaperCostPolicy `json:"paper,omitempty"`
	MaxTracked   int              `json:"max_tracked"`
	MaxAgeMS     int64            `json:"max_age_ms"`
	EvaluationMS int64            `json:"evaluation_ms"`
	Costs        []ResearchCost   `json:"costs"`
	SwapAudit    SwapAuditPolicy  `json:"swap_audit"`
}
type ResearchCost struct {
	Token        string `json:"token"`
	NetworkFee   string `json:"network_fee_in_token"`
	LoanFeePips  uint64 `json:"loan_fee_pips"`
	LoanFixed    string `json:"loan_fixed"`
	SafetyBPS    uint64 `json:"safety_bps"`
	MinNetProfit string `json:"min_net_profit"`
}
type ResearchEconomics struct {
	Status      EconomicsStatus   `json:"net_status"`
	Calibration *PaperCalibration `json:"calibration,omitempty"`
	Basis       string            `json:"basis"`
	Gross       *big.Int          `json:"gross"`
	NetworkFee  *big.Int          `json:"network_fee,omitempty"`
	LoanFee     *big.Int          `json:"loan_fee,omitempty"`
	Safety      *big.Int          `json:"safety,omitempty"`
	Net         *big.Int          `json:"estimated_net,omitempty"`
	Eligible    bool              `json:"eligible"`
}

func LoadResearchPolicy(path string) (ResearchPolicy, error) {
	policy := ResearchPolicy{MaxTracked: 128, MaxAgeMS: 10000, EvaluationMS: 20, SwapAudit: SwapAuditPolicy{MaxSwaps: 16, EvaluationMS: 50, EveryBlocks: 1}}
	if path == "" {
		return policy, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return policy, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return policy, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return policy, fmt.Errorf("research config trailing JSON")
	}
	if policy.Paper != nil {
		policy.Paper.defaults()
	}
	return policy, policy.Validate()
}
func (p ResearchPolicy) Validate() error {
	if p.SwapAudit.MaxSwaps < 0 || p.SwapAudit.MaxSwaps > 256 || p.SwapAudit.MaxSwaps > 0 && (p.SwapAudit.EvaluationMS < 1 || p.SwapAudit.EvaluationMS > 1000 || p.SwapAudit.EveryBlocks == 0) {
		return fmt.Errorf("invalid swap audit limits")
	}
	if p.MaxTracked < 1 || p.MaxTracked > 1024 || p.MaxAgeMS < 1 || p.MaxAgeMS > 60000 || p.EvaluationMS < 1 || p.EvaluationMS > 1000 || len(p.Costs) > 64 {
		return fmt.Errorf("invalid research limits")
	}
	if p.Paper != nil {
		if len(p.Costs) > 0 {
			return fmt.Errorf("paper receipt scenario and fixed costs are mutually exclusive")
		}
		if err := p.Paper.validate(); err != nil {
			return err
		}
	}
	seen := map[domain.Address]bool{}
	for _, cost := range p.Costs {
		token, err := domain.ParseAddress(cost.Token)
		if err != nil || seen[token] || cost.LoanFeePips >= 1000000 || cost.SafetyBPS > 10000 {
			return fmt.Errorf("invalid research cost asset")
		}
		seen[token] = true
		for _, text := range []string{cost.NetworkFee, cost.LoanFixed, cost.MinNetProfit} {
			value, ok := new(big.Int).SetString(text, 10)
			if !ok || value.Sign() < 0 || value.BitLen() > 256 {
				return fmt.Errorf("research costs require explicit unsigned token-unit amounts")
			}
		}
	}
	return nil
}
func (p ResearchPolicy) Economics(token domain.Address, input, gross *big.Int) ResearchEconomics {
	result := ResearchEconomics{Status: NetUnknown, Basis: "gross_only", Gross: new(big.Int).Set(gross)}
	for _, cost := range p.Costs {
		asset, err := domain.ParseAddress(cost.Token)
		if err != nil || asset != token {
			continue
		}
		result.Basis = "configured_cost_budget"
		result.NetworkFee, _ = new(big.Int).SetString(cost.NetworkFee, 10)
		result.LoanFee = ceilResearch(new(big.Int).Mul(input, new(big.Int).SetUint64(cost.LoanFeePips)), 1000000)
		fixed, _ := new(big.Int).SetString(cost.LoanFixed, 10)
		result.LoanFee.Add(result.LoanFee, fixed)
		result.Safety = ceilResearch(new(big.Int).Mul(input, new(big.Int).SetUint64(cost.SafetyBPS)), 10000)
		result.Net = new(big.Int).Sub(gross, result.NetworkFee)
		result.Net.Sub(result.Net, result.LoanFee)
		result.Net.Sub(result.Net, result.Safety)
		minimum, _ := new(big.Int).SetString(cost.MinNetProfit, 10)
		result.classify(minimum)
		break
	}
	return result
}
func ceilResearch(value *big.Int, divisor int64) *big.Int {
	result, remainder := new(big.Int), new(big.Int)
	result.QuoRem(value, big.NewInt(divisor), remainder)
	if remainder.Sign() != 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}
