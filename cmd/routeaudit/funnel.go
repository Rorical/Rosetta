package main

import (
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/backtest"
)

// The denominator is positive swap surplus less the observed whole-transaction
// network fee, with a public reaction window. Loan/settlement costs and private
// submission time remain unverified; this is not realized wallet-profit recall.
type recallFunnel struct {
	NotObservable                  int            `json:"not_observable"`
	UnknownPaidFee                 int            `json:"observable_paid_fee_unknown"`
	NonPositiveAfterPaidFee        int            `json:"observable_non_positive_after_paid_network_fee"`
	ObservablePositiveAfterPaidFee int            `json:"observable_positive_after_paid_network_fee"`
	Quoted                         int            `json:"quoted_before_deadline"`
	Optimized                      int            `json:"optimized_before_deadline"`
	PaperSampled                   int            `json:"execution_paper_sampled"`
	PaperPositive                  int            `json:"execution_paper_discovery_positive"`
	PaperReady                     int            `json:"execution_paper_positive_finished_before_deadline"`
	PaperSkipped                   int            `json:"execution_paper_skip_witness"`
	PaperUnknown                   int            `json:"execution_paper_not_observed_or_not_provided"`
	Reasons                        map[string]int `json:"unconfirmed_reasons"`
	Largest                        []missedRoute  `json:"largest_unconfirmed_after_paid_network_fee"`
}

func (a *automaticAudit) recordFunnel(c *cycleAudit, v backtest.Visibility, status, optimized string, cause auditAttribution, paidNet *big.Int, paper executionAuditResult) {
	if a.funnel == nil {
		a.funnel = map[string]*recallFunnel{}
	}
	key := c.cycle.Cycle.Start.Hex()
	row := a.funnel[key]
	if row == nil {
		row = &recallFunnel{Reasons: map[string]int{}}
		a.funnel[key] = row
	}
	if v.Status != "observable" {
		row.NotObservable++
		return
	}
	if paidNet == nil {
		row.UnknownPaidFee++
		return
	}
	if paidNet.Sign() <= 0 {
		row.NonPositiveAfterPaidFee++
		return
	}
	row.ObservablePositiveAfterPaidFee++
	if status == "reported" {
		row.Quoted++
	}
	if optimized == "reported" {
		row.Optimized++
	}
	reason, stage := cause.Reason, cause.Stage
	switch paper.Status {
	case "sampled":
		row.PaperSampled++
		if paper.BaselinePositive > 0 {
			row.PaperPositive++
		}
		if paper.ReadyBeforeDeadline > 0 {
			row.PaperReady++
		}
	case "sampling_skipped":
		row.PaperSkipped++
	default:
		row.PaperUnknown++
	}
	if status == "reported" && optimized == "reported" {
		stage = "execution_paper"
		switch {
		case paper.ReadyBeforeDeadline > 0:
			return
		case paper.BaselinePositive > 0:
			reason = "paper_confirmation_after_window"
		case paper.Status == "sampling_skipped":
			reason = "paper_sampling_skipped"
		case paper.BaselineNonPositive > 0:
			reason = "paper_discovery_non_positive_or_reverted"
		case paper.Status == "sampled":
			reason = "paper_discovery_unknown"
		default:
			reason = "paper_" + paper.Status
		}
	}
	row.Reasons[reason]++
	row.Largest = append(row.Largest, missedRoute{Transaction: c.cycle.Transaction, Block: a.block.Block.Number, Reason: reason, Stage: stage, Gross: new(big.Int).Set(c.cycle.ActualGross), PaidNet: new(big.Int).Set(paidNet)})
	sort.Slice(row.Largest, func(i, j int) bool {
		x, y := row.Largest[i], row.Largest[j]
		if n := x.PaidNet.Cmp(y.PaidNet); n != 0 {
			return n > 0
		}
		return x.Transaction.Hex() < y.Transaction.Hex()
	})
	if len(row.Largest) > 20 {
		row.Largest = row.Largest[:20]
	}
}
