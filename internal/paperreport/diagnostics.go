package paperreport

import (
	"encoding/json"
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/domain"
)

type Stratum struct {
	Asset       domain.Address `json:"asset"`
	Hops        int            `json:"hops"`
	Protocols   string         `json:"protocol_counts"`
	GrossDigits int            `json:"gross_raw_digits"`
	Supported   bool           `json:"executor_pool_kinds_supported"`
}
type SamplingBucket struct {
	Stratum  Stratum           `json:"stratum"`
	Reasons  map[string]uint64 `json:"reasons"`
	MaxGross *big.Int          `json:"max_quoted_gross_raw"`
}
type Selection struct {
	WaitNS  int64   `json:"sampling_wait_ns"`
	Mode    string  `json:"mode"`
	Stratum Stratum `json:"stratum"`
}
type StratumReport struct {
	Stratum         Stratum           `json:"stratum"`
	Selected        int               `json:"selected_groups"`
	Status          map[string]int    `json:"discovery_execution_status"`
	Modes           map[string]int    `json:"selection_modes"`
	Skipped         map[string]uint64 `json:"skipped_quote_observations"`
	MaxSkippedGross *big.Int          `json:"max_skipped_gross_raw,omitempty"`
	GrossError      *big.Int          `json:"actual_minus_quoted_gross_raw_sum"`
}
type bidSample struct {
	Status    string   `json:"status"`
	Basis     string   `json:"valuation_basis"`
	Max       *big.Int `json:"max_positive_priority_fee_wei"`
	Scenarios []struct {
		Tip      *big.Int `json:"priority_fee_wei"`
		Net      *big.Int `json:"net_native_wei"`
		Fits     bool     `json:"fits_fee_cap"`
		Positive bool     `json:"positive_net"`
	} `json:"scenarios"`
}
type BidScenarioCounts struct {
	DelayMS     int64  `json:"delay_ms"`
	Tip         string `json:"priority_fee_wei"`
	Samples     int    `json:"samples"`
	Positive    int    `json:"positive_within_fee_cap"`
	NonPositive int    `json:"non_positive"`
	CapExceeded int    `json:"fee_cap_exceeded"`
}
type diagnostics struct {
	strata map[Stratum]*StratumReport
	bids   map[string]*BidScenarioCounts
}

func (d *diagnostics) stratum(s Stratum) *StratumReport {
	if d.strata == nil {
		d.strata = map[Stratum]*StratumReport{}
	}
	row := d.strata[s]
	if row == nil {
		row = &StratumReport{Stratum: s, Status: map[string]int{}, Modes: map[string]int{}, Skipped: map[string]uint64{}, GrossError: new(big.Int)}
		d.strata[s] = row
	}
	return row
}
func (d *diagnostics) skipped(rows []SamplingBucket) {
	for _, b := range rows {
		row := d.stratum(b.Stratum)
		for reason, count := range b.Reasons {
			row.Skipped[reason] += count
		}
		if b.MaxGross != nil && (row.MaxSkippedGross == nil || b.MaxGross.Cmp(row.MaxSkippedGross) > 0) {
			row.MaxSkippedGross = new(big.Int).Set(b.MaxGross)
		}
	}
}
func (d *diagnostics) add(s sample) {
	if s.DelayMS == 0 && s.Selection != nil {
		row := d.stratum(s.Selection.Stratum)
		row.Selected++
		row.Status[s.Status]++
		row.Modes[s.Selection.Mode]++
		if s.StateBasis == "discovery_snapshot" && s.Status == "success" && s.Profit != nil && s.LoanFee != nil && s.QuotedGross != nil {
			delta := new(big.Int).Add(s.Profit, s.LoanFee)
			delta.Sub(delta, s.QuotedGross)
			row.GrossError.Add(row.GrossError, delta)
		}
	}
	if s.Bidding == nil || s.Status != "success" || (s.StateBasis != "discovery_snapshot" && s.StateBasis != "later_observed_prefix") {
		return
	}
	if d.bids == nil {
		d.bids = map[string]*BidScenarioCounts{}
	}
	for _, b := range s.Bidding.Scenarios {
		if b.Tip == nil || b.Net == nil {
			continue
		}
		key, _ := json.Marshal([]any{s.DelayMS, b.Tip.String()})
		row := d.bids[string(key)]
		if row == nil {
			row = &BidScenarioCounts{DelayMS: s.DelayMS, Tip: b.Tip.String()}
			d.bids[string(key)] = row
		}
		row.Samples++
		if !b.Fits {
			row.CapExceeded++
		} else if b.Net.Sign() > 0 {
			row.Positive++
		} else {
			row.NonPositive++
		}
	}
}
func (d *diagnostics) finish(report *Report) {
	for _, row := range d.strata {
		report.Sampling = append(report.Sampling, *row)
	}
	sort.Slice(report.Sampling, func(i, j int) bool {
		a, _ := json.Marshal(report.Sampling[i].Stratum)
		b, _ := json.Marshal(report.Sampling[j].Stratum)
		return string(a) < string(b)
	})
	for _, row := range d.bids {
		report.Bids = append(report.Bids, *row)
	}
	sort.Slice(report.Bids, func(i, j int) bool {
		a, b := report.Bids[i], report.Bids[j]
		if a.DelayMS != b.DelayMS {
			return a.DelayMS < b.DelayMS
		}
		x, _ := new(big.Int).SetString(a.Tip, 10)
		y, _ := new(big.Int).SetString(b.Tip, 10)
		return x.Cmp(y) < 0
	})
}
