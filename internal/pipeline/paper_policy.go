package pipeline

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// PaperCostPolicy enables a receipt-based cost scenario and empirical quote
// reserve. It is research evidence, never an execution fee quotation.
type PaperCostPolicy struct {
	SamplePath         string `json:"sample_path,omitempty"`
	QuoteHistoryBlocks uint64 `json:"quote_history_blocks,omitempty"`
	WrappedNative      string `json:"wrapped_native"`
	HistoryBlocks      uint64 `json:"history_blocks"`
	MinSamples         int    `json:"min_samples"`
	FundingBasis       string `json:"funding_basis"`
	LoanFeePips        uint64 `json:"loan_fee_pips"`
	LoanFixed          string `json:"loan_fixed"`
	MinNetProfit       string `json:"min_net_profit"`
}

func (p *PaperCostPolicy) defaults() {
	if p.HistoryBlocks == 0 {
		p.HistoryBlocks = 128
	}
	if p.QuoteHistoryBlocks == 0 {
		p.QuoteHistoryBlocks = 4096
	}
	if p.MinSamples == 0 {
		p.MinSamples = 3
	}
	if p.LoanFixed == "" {
		p.LoanFixed = "0"
	}
	if p.MinNetProfit == "" {
		p.MinNetProfit = "0"
	}
}
func (p PaperCostPolicy) validate() error {
	address, err := domain.ParseAddress(p.WrappedNative)
	if err != nil || address == (domain.Address{}) || p.HistoryBlocks < 1 || p.HistoryBlocks > 4096 || p.QuoteHistoryBlocks > 65536 || p.MinSamples < 1 || p.MinSamples > 64 || p.LoanFeePips >= 1000000 {
		return fmt.Errorf("invalid paper calibration policy")
	}
	if p.FundingBasis != "own_capital_scenario" && p.FundingBasis != "configured_loan_scenario" {
		return fmt.Errorf("paper funding basis must be explicit")
	}
	for _, text := range []string{p.LoanFixed, p.MinNetProfit} {
		n, ok := new(big.Int).SetString(text, 10)
		if !ok || n.Sign() < 0 || n.BitLen() > 256 {
			return fmt.Errorf("invalid paper cost amount")
		}
	}
	if p.FundingBasis == "own_capital_scenario" && (p.LoanFeePips != 0 || p.LoanFixed != "0") {
		return fmt.Errorf("own capital paper scenario has loan fees")
	}
	return nil
}
