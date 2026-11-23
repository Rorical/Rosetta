package main

import (
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
)

type auditAttribution struct {
	StateApplyDelayNS       *int64 `json:"state_apply_delay_ns,omitempty"`
	Stage                   string `json:"stage"`
	Evidence                string `json:"evidence"`
	Reason                  string `json:"reason"`
	QuoteAfterAvailableNS   *int64 `json:"quote_after_available_ns,omitempty"`
	PublicationAfterQuoteNS *int64 `json:"publication_after_quote_ns,omitempty"`
	DeadlineSlackNS         *int64 `json:"publication_deadline_slack_ns,omitempty"`
}

func attribution(c *cycleAudit, v backtest.Visibility, reason string, quoted, published int64) auditAttribution {
	out := auditAttribution{Stage: "unknown", Evidence: "insufficient_online_evidence", Reason: reason}
	switch reason {
	case "missing_pool", "missing_token":
		out.Stage, out.Evidence = "coverage", "captured_pool_inventory"
	case "missing_ticks", "unsupported_protocol", "quote_failed":
		out.Stage, out.Evidence = "state_or_model", "local_replay"
	case "reported":
		out.Stage, out.Evidence = "reported", "canonical_prefix_and_online_publication"
	case "publication_late":
		out.Stage, out.Evidence = "publication", "online_timestamps"
	case "amount_optimization_after_window", "optimized_quote_not_observed":
		out.Stage, out.Evidence = "amount_optimization", "online_quote_stages"
	case "published_late", "search_finished_after_window", "search_canceled_or_superseded":
		out.Stage, out.Evidence = "search_timing", "online_trace"
	case "primary_catalog_absent_online_catalog_unknown":
		out.Stage, out.Evidence = "catalog_unknown", "primary_catalog_only"
	case "depth_limit", "unfunded_start", "input_outside_bounds":
		out.Stage, out.Evidence = "search_policy", "captured_policy_replay"
	case "coarse_ranking_limit", "path_not_dirty", "amount_search_or_exact_quote":
		out.Stage, out.Evidence = "search_replay", "reconstructed_primary_catalog"
	default:
		if v.Status != "observable" {
			out.Stage, out.Evidence = "visibility", "canonical_prefix_observations"
		}
	}
	if v.Received > 0 && v.Available >= v.Received {
		n := v.Available - v.Received
		out.StateApplyDelayNS = &n
	}
	if v.Available > 0 && quoted >= v.Available {
		n := quoted - v.Available
		out.QuoteAfterAvailableNS = &n
	}
	if quoted > 0 && published >= quoted {
		n := published - quoted
		out.PublicationAfterQuoteNS = &n
	}
	if published > 0 && v.Deadline > 0 {
		n := v.Deadline - published
		out.DeadlineSlackNS = &n
	}
	return out
}

type missedRoute struct {
	Transaction domain.Hash `json:"transaction"`
	Block       uint64      `json:"block"`
	Reason      string      `json:"reason"`
	Stage       string      `json:"stage"`
	Gross       *big.Int    `json:"swap_surplus_raw"`
	PaidNet     *big.Int    `json:"surplus_after_paid_network_fee_raw,omitempty"`
}
type omissionAsset struct {
	Counts        map[string]int      `json:"reasons"`
	GrossByReason map[string]*big.Int `json:"swap_surplus_by_reason_raw"`
	Largest       []missedRoute       `json:"largest_unreported"`
}

func (a *automaticAudit) recordOmission(c *cycleAudit, row auditAttribution, paidNet *big.Int) {
	if a.omissions == nil {
		a.omissions = map[string]*omissionAsset{}
	}
	key := c.cycle.Cycle.Start.Hex()
	asset := a.omissions[key]
	if asset == nil {
		asset = &omissionAsset{Counts: map[string]int{}, GrossByReason: map[string]*big.Int{}}
		a.omissions[key] = asset
	}
	asset.Counts[row.Reason]++
	if asset.GrossByReason[row.Reason] == nil {
		asset.GrossByReason[row.Reason] = new(big.Int)
	}
	asset.GrossByReason[row.Reason].Add(asset.GrossByReason[row.Reason], c.cycle.ActualGross)
	if row.Reason == "reported" {
		return
	}
	asset.Largest = append(asset.Largest, missedRoute{c.cycle.Transaction, a.block.Block.Number, row.Reason, row.Stage, new(big.Int).Set(c.cycle.ActualGross), paidNet})
	sort.Slice(asset.Largest, func(i, j int) bool {
		x, y := asset.Largest[i], asset.Largest[j]
		if d := x.Gross.Cmp(y.Gross); d != 0 {
			return d > 0
		}
		return x.Transaction.Hex() < y.Transaction.Hex()
	})
	if len(asset.Largest) > 20 {
		asset.Largest = asset.Largest[:20]
	}
}

// Unknown swap transactions stay outside the profit denominator. Retain bounded
// identities for pool inventory expansion; a swap log is not proof of arbitrage.
type coverageGap struct {
	Transaction domain.Hash    `json:"transaction"`
	Block       uint64         `json:"block"`
	Logs        []swapIdentity `json:"swap_logs"`
}
type swapIdentity struct {
	Address domain.Address `json:"address"`
	Topics  []domain.Hash  `json:"topics"`
}

func (a *automaticAudit) recordUnknownSwaps(tx ingest.Transaction) {
	if len(a.coverageGaps) >= 128 {
		a.coverageGapOverflow++
		return
	}
	row := coverageGap{Transaction: tx.Hash, Block: a.block.Block.Number}
	for _, log := range tx.Logs {
		if len(log.Topics) > 0 && dex.IsSwapTopic(log.Topics[0]) {
			row.Logs = append(row.Logs, swapIdentity{log.Address, append([]domain.Hash(nil), log.Topics...)})
		}
	}
	a.coverageGaps = append(a.coverageGaps, row)
}

func optimizedPublication(stageObserved bool, published, deadline int64) string {
	if deadline == 0 {
		return "window_unknown"
	}
	if !stageObserved {
		return "stage_unknown"
	}
	if published == 0 {
		return "not_observed"
	}
	if published < deadline {
		return "reported"
	}
	return "published_late"
}
