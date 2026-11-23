package main

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/state"
)

type assetRecall struct {
	Observable    int      `json:"observable"`
	Reported      int      `json:"reported"`
	Gross         *big.Int `json:"observed_swap_gross"`
	ReportedGross *big.Int `json:"reported_swap_gross"`
}
type cycleAudit struct {
	cycle    backtest.ClosedCycle
	positive map[uint64]bool
	// parentPositive is the route after each frame end of the parent block,
	// quoted only when the route is positive at its own block's start.
	parentPositive   map[uint64]bool
	diagnosticParent bool
	quoteErrors      map[string]int
	preGross         *big.Int
	paidFeeInAsset   *big.Int
	paidFeeBasis     string
	diagnostic       *backtest.Diagnostic
	diagnosticPrefix uint64
	diagnosticRoute  backtest.KnownRoute
}
type automaticAudit struct {
	execution           executionTrace
	funnel              map[string]*recallFunnel
	coverageGaps        []coverageGap
	coverageGapOverflow int
	omissions           map[string]*omissionAsset
	metadata            []backtest.PoolMetadata
	canonical           map[domain.Hash]bool
	trace               traceData
	output              *json.Encoder
	policy              pipeline.SearchPolicy
	costs               pipeline.ResearchPolicy
	limit, total        int
	chain               uint64
	block               backtest.Record
	current             []*cycleAudit
	dirty               map[uint64][]domain.PoolKey
	counts, excluded    map[string]int
	gross               map[string]*assetRecall
	truncated           bool
	headers             map[domain.Hash]domain.BlockRef
	// views are the current block's states at its frame ends; parent keeps
	// the previous block's, for routes already positive at a block's start.
	views    map[uint64]state.View
	parent   *parentBlock
	topology *pipeline.Topology
}

type parentBlock struct {
	record backtest.Record
	dirty  map[uint64][]domain.PoolKey
	views  map[uint64]state.View
}

func (a *automaticAudit) startBlock(record backtest.Record) {
	a.parent = nil
	if a.views != nil && a.block.Block.Hash == record.Block.ParentHash {
		a.parent = &parentBlock{record: a.block, dirty: a.dirty, views: a.views}
	}
	a.views = map[uint64]state.View{}
	a.block = record
	a.rememberAnchor(record.Block)
	a.current = nil
	a.dirty = map[uint64][]domain.PoolKey{}
	if !a.canonical[record.Block.Hash] {
		a.excluded["block_not_on_retained_chain"] += len(record.Transactions)
		return
	}
	for _, tx := range record.Transactions {
		cycle, reason := backtest.ExtractClosedCycle(record.Block, tx, a.metadata, backtest.SwapDecoder{Matches: dex.IsSwapTopic, Amounts: dex.DecodeSwapAmounts})
		if reason != "" {
			a.excluded[reason]++
			if reason == "unknown_swap_pool" {
				a.recordUnknownSwaps(tx)
			}
			continue
		}
		if a.total >= a.limit {
			a.truncated = true
			continue
		}
		a.total++
		a.current = append(a.current, &cycleAudit{cycle: cycle, positive: map[uint64]bool{}, quoteErrors: map[string]int{}})
	}
}
func (a *automaticAudit) observe(ctx context.Context, view state.View, topology *pipeline.Topology, ref domain.BlockRef, before uint64, dirty []domain.PoolKey) error {
	a.dirty[before] = slices.Clone(dirty)
	a.topology = topology
	if a.costs.Paper != nil {
		wrapped, _ := domain.ParseAddress(a.costs.Paper.WrappedNative)
		for _, c := range a.current {
			if before == c.cycle.BeforeTx && c.cycle.Fees != nil {
				c.paidFeeInAsset, c.paidFeeBasis, _ = pipeline.NativeFeeInRoute(view, c.cycle.Cycle, wrapped, c.cycle.Fees.Total)
			}
		}
	}
	frames := a.canonicalFrames(ref)
	for _, f := range frames {
		if f.Count > 0 && f.First+f.Count == before && f.Matches(a.block.Transactions) {
			a.views[before] = view
			break
		}
	}
	for _, c := range a.current {
		if before > c.cycle.BeforeTx {
			continue
		}
		quote, err := topology.Engine.QuoteCycle(ctx, view, c.cycle.Cycle, c.cycle.Input)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.quoteErrors[err.Error()]++
		} else {
			c.positive[before] = quote.Net.Sign() > 0
			if before == c.cycle.BeforeTx {
				c.preGross = new(big.Int).Set(quote.Net)
			}
		}
		// Diagnose at the earliest observable whole-frame prefix, with the entire
		// frame's dirty union. A pre-competitor fallback stays explicitly separate.
		eligible := false
		var frameDirty []domain.PoolKey
		for _, f := range frames {
			if f.First+f.Count != before || !f.Matches(a.block.Transactions) || !c.positive[before] {
				continue
			}
			deadline := int64(0)
			for _, last := range frames {
				if last.Matches(a.block.Transactions) && last.Count > 0 && last.First <= c.cycle.BeforeTx && c.cycle.BeforeTx-last.First < last.Count && (deadline == 0 || last.Received < deadline) {
					deadline = last.Received
				}
			}
			if deadline > f.Applied {
				eligible = true
				for prefix := f.First + 1; prefix <= before; prefix++ {
					frameDirty = append(frameDirty, a.dirty[prefix]...)
				}
			}
		}
		if c.diagnostic != nil || !eligible && before != c.cycle.BeforeTx {
			continue
		}
		if !eligible {
			frameDirty = dirty
		}
		if err := a.diagnose(ctx, view, topology, c, before, frameDirty); err != nil {
			return err
		}
	}
	return nil
}

// diagnose replays the search stages for c at view, with the pools a frame
// changed as its dirty set.
func (a *automaticAudit) diagnose(ctx context.Context, view state.View, topology *pipeline.Topology, c *cycleAudit, before uint64, frameDirty []domain.PoolKey) error {
	activeTokens := map[domain.Address]bool{}
	for _, key := range view.PoolKeys() {
		p, _ := view.Pool(key)
		activeTokens[p.Token0] = true
		activeTokens[p.Token1] = true
	}
	known := diagnosticFunding(c.cycle, a.policy)
	known.BeforeTx = before
	diagnostic, err := backtest.Diagnose(ctx, view, topology.Table, topology.Engine, known, backtest.DiagnosticOptions{Diverse: true, ActiveTokens: activeTokens, Model: dex.RankingModel, Depth: a.policy.Depth, Truncated: topology.Truncated, Dirty: frameDirty, Bounds: a.policy.Bounds, SwapFee: dex.SwapFee, Costs: func(token domain.Address, input, gross *big.Int) (*big.Int, string) {
		cost := a.costs.Economics(token, input, gross)
		return cost.Net, cost.Basis
	}})
	if err != nil {
		return err
	}
	c.diagnosticRoute = known
	c.diagnostic = &diagnostic
	c.diagnosticPrefix = before
	return nil
}

// parentPrefixes quotes c at the parent block's frame ends when c is positive
// at its own block's start, so its window can begin in the parent.
func (a *automaticAudit) parentPrefixes(ctx context.Context, c *cycleAudit, frames []backtest.PrefixEvidence) (*backtest.ParentPrefixes, error) {
	if a.parent == nil || !c.positive[0] {
		return nil, nil
	}
	c.parentPositive = map[uint64]bool{}
	for _, f := range frames {
		end := f.First + f.Count
		view, ok := a.parent.views[end]
		if !ok || !f.Matches(a.parent.record.Transactions) {
			continue
		}
		quote, err := a.topology.Engine.QuoteCycle(ctx, view, c.cycle.Cycle, c.cycle.Input)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.parentPositive[end] = err == nil && quote.Net.Sign() > 0
	}
	return &backtest.ParentPrefixes{Transactions: a.parent.record.Transactions, Frames: frames, Positive: c.parentPositive}, nil
}

// diagnoseParent diagnoses c at the parent frame its window starts at, with
// that frame's changed pools: the child's prefix holds only the start-of-block
// transaction, which changes none.
func (a *automaticAudit) diagnoseParent(ctx context.Context, c *cycleAudit, v backtest.Visibility, frames []backtest.PrefixEvidence) error {
	for _, f := range frames {
		if f.Version != v.Version || f.First+f.Count != v.Prefix || !f.Matches(a.parent.record.Transactions) {
			continue
		}
		var dirty []domain.PoolKey
		for prefix := f.First + 1; prefix <= v.Prefix; prefix++ {
			dirty = append(dirty, a.parent.dirty[prefix]...)
		}
		c.diagnosticParent = true
		return a.diagnose(ctx, a.parent.views[v.Prefix], a.topology, c, v.Prefix, dirty)
	}
	return nil
}

// visibleFrame is the frame at version after which c was positive and
// visible: in the competitor's block before it, or in the parent block when
// the window starts there.
func (a *automaticAudit) visibleFrame(c *cycleAudit, v backtest.Visibility, version domain.StateVersion) (backtest.PrefixEvidence, bool) {
	for _, f := range a.canonicalFrames(a.block.Block) {
		end := f.First + f.Count
		if f.Version == version && f.Matches(a.block.Transactions) && end <= c.cycle.BeforeTx && c.positive[end] && f.Applied >= v.Available {
			return f, true
		}
	}
	if v.Parent && a.parent != nil {
		for _, f := range a.canonicalFrames(a.parent.record.Block) {
			if f.Version == version && f.Matches(a.parent.record.Transactions) && c.parentPositive[f.First+f.Count] && f.Applied >= v.Available {
				return f, true
			}
		}
	}
	return backtest.PrefixEvidence{}, false
}

func (a *automaticAudit) endBlock(ctx context.Context) error {
	frames := a.canonicalFrames(a.block.Block)
	var parentFrames []backtest.PrefixEvidence
	if a.parent != nil {
		parentFrames = a.canonicalFrames(a.parent.record.Block)
	}
	tokens := func(key domain.PoolKey) (domain.Address, domain.Address, bool) {
		for _, m := range a.metadata {
			if m.Key == key {
				return m.Token0, m.Token1, true
			}
		}
		return domain.Address{}, domain.Address{}, false
	}
	for _, c := range a.current {
		parent, err := a.parentPrefixes(ctx, c, parentFrames)
		if err != nil {
			return err
		}
		visibility := backtest.ReactionWindow(a.block.Transactions, c.cycle.BeforeTx, frames, c.positive, a.trace.gaps, parent)
		if visibility.Parent {
			if err := a.diagnoseParent(ctx, c, visibility, parentFrames); err != nil {
				return err
			}
		}
		a.counts[visibility.Status]++
		status := "not_evaluated"
		var firstQuoted, firstPublished, optimizedQuoted, optimizedPublished int64
		var firstStage arb.QuoteStage
		stageObserved := false
		var matchBasis domain.StateVersion
		if visibility.Status == "observable" {
			status = "not_reported"
			if !a.trace.candidateFile {
				status = "candidate_capture_missing"
			}
			for _, q := range a.trace.candidates {
				if q.Opportunity.Net == nil || q.Opportunity.Net.Sign() <= 0 || !graph.SameDirectedCycle(q.Opportunity.Cycle, c.cycle.Cycle, tokens) {
					continue
				}
				if _, valid := a.visibleFrame(c, visibility, q.Timing.BasisVersion); !valid {
					continue
				}
				if q.Timing.Stage != "" {
					stageObserved = true
				}
				if q.Timing.Stage == arb.QuoteOptimized && (optimizedPublished == 0 || q.Timing.RecordedAtNS < optimizedPublished) {
					optimizedPublished = q.Timing.RecordedAtNS
					optimizedQuoted = q.Timing.QuotedAtNS
				}
				if firstPublished == 0 || q.Timing.RecordedAtNS < firstPublished {
					firstPublished = q.Timing.RecordedAtNS
					firstQuoted = q.Timing.QuotedAtNS
					firstStage = q.Timing.Stage
					matchBasis = q.Timing.BasisVersion
				}
			}
			if firstPublished > 0 {
				status = "published_late"
				if firstPublished < visibility.Deadline {
					status = "reported"
				} else if firstQuoted > 0 && firstQuoted < visibility.Deadline {
					status = "publication_late"
				}
			}
			a.counts[status]++
			asset := c.cycle.Cycle.Start.Hex()
			recall := a.gross[asset]
			if recall == nil {
				recall = &assetRecall{Gross: new(big.Int), ReportedGross: new(big.Int)}
				a.gross[asset] = recall
			}
			recall.Observable++
			recall.Gross.Add(recall.Gross, c.cycle.ActualGross)
			if status == "reported" {
				recall.Reported++
				recall.ReportedGross.Add(recall.ReportedGross, c.cycle.ActualGross)
			}
		}
		var paidNet *big.Int
		if c.paidFeeInAsset != nil {
			paidNet = new(big.Int).Sub(c.cycle.ActualGross, c.paidFeeInAsset)
		}
		var delta *big.Int
		if c.preGross != nil {
			delta = new(big.Int).Sub(c.preGross, c.cycle.ActualGross)
		}
		reason := a.missReason(c, visibility, status)
		optimizedStatus := optimizedPublication(stageObserved, optimizedPublished, visibility.Deadline)
		if status == "reported" && optimizedStatus == "published_late" {
			reason = "amount_optimization_after_window"
		}
		if status == "reported" && optimizedStatus == "not_observed" {
			reason = "optimized_quote_not_observed"
		}
		cause := attribution(c, visibility, reason, firstQuoted, firstPublished)
		a.recordOmission(c, cause, paidNet)
		paper := a.executionAudit(c, visibility, a.execution, tokens)
		a.recordFunnel(c, visibility, status, optimizedStatus, cause, paidNet, paper)
		if err := a.output.Encode(map[string]any{"execution_paper": paper, "attribution": cause, "first_quote_stage": firstStage, "optimized_status": optimizedStatus, "first_optimized_quoted_at_ns": optimizedQuoted, "first_optimized_published_at_ns": optimizedPublished, "paid_priority_fee_wei": c.cycle.Fees.PriorityFee(), "type": "opportunity_audit", "chain_id": a.chain, "block": a.block.Block, "cycle": c.cycle, "visibility": visibility, "online_status": status, "miss_reason": reason, "first_quoted_at_ns": firstQuoted, "first_published_at_ns": firstPublished, "published_basis": matchBasis, "diagnostic_before_tx": c.diagnosticPrefix, "diagnostic_parent_block": c.diagnosticParent, "diagnostic_route": c.diagnosticRoute, "diagnostic": c.diagnostic, "quote_errors": c.quoteErrors, "pre_competitor_model_delta": delta, "paid_network_fee_in_asset": c.paidFeeInAsset, "paid_fee_conversion_basis": c.paidFeeBasis, "swap_surplus_after_paid_network_fee": paidNet, "search_observations": a.trace.searches[visibility.Version], "catalog_scope": "primary_table_only_supplemental_catalog_not_captured"}); err != nil {
			return err
		}
	}
	return nil
}
func (a *automaticAudit) summary() error {
	return a.output.Encode(map[string]any{"type": "opportunity_summary", "chain_id": a.chain, "cycles": a.total, "truncated": a.truncated, "counts": a.counts, "excluded_transactions": a.excluded, "by_surplus_asset": a.gross, "omissions_by_asset": a.omissions, "recall_funnel_by_asset": a.funnel, "recall_funnel_basis": "observable_swap_surplus_after_paid_network_fee; funding_and_settlement_unverified", "unknown_swap_transactions": a.coverageGaps, "unknown_swap_examples_omitted": a.coverageGapOverflow, "basis": "supported_simple_swap_surplus; funding_settlement_gas_unverified; submission_time_unknown; local_captures_only"})
}

func (a *automaticAudit) canonicalFrames(ref domain.BlockRef) []backtest.PrefixEvidence {
	var frames []backtest.PrefixEvidence
	for _, f := range a.trace.frames[ref.Number] {
		if a.hasAnchor(ref, f.Version.Anchor) {
			frames = append(frames, f)
		}
	}
	return frames
}

func (a *automaticAudit) missReason(c *cycleAudit, v backtest.Visibility, status string) string {
	// A missing pool cannot be quoted into a positive visibility window. Keep
	// that coverage failure visible without inventing an actionable window.
	if c.diagnostic != nil {
		for _, reason := range c.diagnostic.Reasons {
			if reason == "missing_pool" || reason == "missing_token" {
				return reason
			}
		}
	}
	if v.Status != "observable" {
		if v.Status == "capture_incomplete" {
			var first, last uint64
			for block := range a.trace.frames {
				if first == 0 || block < first {
					first = block
				}
				if block > last {
					last = block
				}
			}
			if first > 0 && a.block.Block.Number < first {
				return "before_feed_capture"
			}
			if last > 0 && a.block.Block.Number > last {
				return "after_feed_capture"
			}
		}
		return v.Status
	}
	if status != "not_reported" {
		return status
	}
	if c.diagnostic != nil {
		for _, reason := range c.diagnostic.Reasons {
			switch reason {
			case "table_truncated", "path_not_enumerated":
				return "primary_catalog_absent_online_catalog_unknown"
			case "missing_pool", "missing_token", "missing_ticks", "unsupported_protocol", "depth_limit", "unfunded_start", "input_outside_bounds", "path_not_dirty", "coarse_ranking_limit", "amount_search_or_exact_quote", "quote_failed":
				return reason
			}
		}
	}
	traces := a.trace.searches[v.Version]
	for _, s := range traces {
		if s.CompletedAtNS > v.Deadline {
			return "search_finished_after_window"
		}
	}
	for _, s := range traces {
		if s.Observation.Outcome == arb.SearchCanceled || s.Observation.Outcome == arb.SearchSuperseded {
			return "search_canceled_or_superseded"
		}
	}
	return "online_decision_not_reproduced"
}

func (a *automaticAudit) rememberAnchor(ref domain.BlockRef) {
	if a.headers == nil {
		a.headers = map[domain.Hash]domain.BlockRef{}
	}
	a.headers[ref.Hash] = ref
}
func (a *automaticAudit) hasAnchor(ref, anchor domain.BlockRef) bool {
	if anchor.Number >= ref.Number {
		return false
	}
	hash := ref.ParentHash
	for number := ref.Number - 1; ; number-- {
		if number == anchor.Number {
			return hash == anchor.Hash
		}
		parent, ok := a.headers[hash]
		if !ok || parent.Number != number {
			return false
		}
		hash = parent.ParentHash
	}
}

// The surplus currency need not be the configured funding currency. Rotation
// keeps the same directed market cycle and uses that leg's observed input.
func diagnosticFunding(c backtest.ClosedCycle, policy pipeline.SearchPolicy) backtest.KnownRoute {
	selected := -1
	for i, token := range c.Tokens {
		bounds, funded := policy.Bounds(token)
		if !funded {
			continue
		}
		if selected < 0 {
			selected = i
		}
		if i < len(c.SwapInputs) && c.SwapInputs[i].Cmp(bounds.Min) >= 0 && c.SwapInputs[i].Cmp(bounds.Max) <= 0 {
			selected = i
			break
		}
	}
	if selected <= 0 || selected >= len(c.SwapInputs) {
		return c.KnownRoute
	}
	known := c.KnownRoute
	known.Cycle.Start = c.Tokens[selected]
	known.Cycle.Pools = append(slices.Clone(c.Cycle.Pools[selected:]), c.Cycle.Pools[:selected]...)
	known.Input = new(big.Int).Set(c.SwapInputs[selected])
	return known
}
