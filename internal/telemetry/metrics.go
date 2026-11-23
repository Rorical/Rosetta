package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/rpc"
)

type Source uint8

const (
	Standard Source = iota
	Fast
	sourceCount
)

type Counter uint8

const (
	Messages Counter = iota
	Receipts
	Logs
	DirtyPools
	SearchSubmitted
	SearchCompleted
	SearchCanceled
	SearchSuperseded
	// SearchCanceledOverlap counts searches canceled before their short
	// routes were sized because newer frames changed every pool that
	// triggered them. SearchOvertaken counts searches that kept ranking
	// when a newer changed frame arrived.
	SearchCanceledOverlap
	SearchOvertaken
	SearchDiscarded
	PathsConsidered
	PathsSelected
	Candidates
	ConfirmationSubmitted
	ConfirmationCompleted
	ConfirmationCanceled
	ConfirmationSuperseded
	ConfirmationRejected
	PlanDiscarded
	ReadyPlans
	QueueOverflow
	StreamInvalidations
	PoolErrors
	BlockMismatch
	CheckpointErrors
	ReconcileErrors
	SearchErrors
	ConfirmationErrors
	RPCHTTPAttempts
	RPCMethodCalls
	RPCRetries
	ExploreSteps
	ExplorePaths
	ExploreTruncated
	// Why the background catalog was truncated: its exploration ran out of
	// steps before covering every pool, or its path cache filled and evicts.
	ExploreBudgetExhausted
	ExploreCacheFull
	SearchCarriedFrames
	SearchPathCanceled
	SearchPathReused
	CandidatesStreamed
	CandidatesDuplicate
	PlansHeld
	RejectOracle
	RejectRoute
	RejectFeeEstimate
	RejectFeeCap
	RejectExpiry
	RejectSimulation
	RejectEconomics
	RejectDispatch
	FeedDisconnects
	FeedRecoveries
	InvalidPayloads
	RPCHTTPErrors
	RPCThrottled
	DiscoveryCompleted
	DiscoveryErrors
	DiscoveryStale
	DiscoveryPoolsAdded
	DiscoveryPoolsSkipped
	TickRepairsDeferred
	// Sizing samples rejected for missing ticks versus pool capacity.
	QuoteOutOfWindow
	QuoteCapacity
	ResearchDropped
	// Reactive funnel: an admitted search result is built into a sealed intent,
	// signed and sent, seen in the feed, then settled at the safe head.
	ReactiveAdmitted
	ReactiveBuilt
	ReactiveSent
	ReactiveRejected
	ReactiveCancels
	ReactiveLanded
	ReactiveSucceeded
	ReactiveReverted
	// Canonical blocks by source: assembled from the flashblock stream or
	// fetched by RPC. Verified and mismatched count the stream blocks the
	// follower compared with an RPC header and receipts.
	CanonicalFlash
	CanonicalRPC
	CanonicalVerified
	CanonicalMismatch
	// Pools whose fee the canonical follower read, by protocol.
	CanonicalFeeSlipstream
	CanonicalFeeV4
	CanonicalFeeOther
	// SearchFramesAbsorbed sums, over searches that started, the frames with
	// changed pools each one searched. A search's long routes (more than
	// three pools) complete, wait for a later search because an earlier one
	// was still ranking its own (deferred), or stop because newer frames
	// changed every pool they were ranked through (preempted).
	// SearchResumed counts searches the worker queued for deferred long routes.
	SearchFramesAbsorbed
	SearchLongCompleted
	SearchLongDeferred
	SearchLongPreempted
	SearchResumed
	// CandidatesRechecked counts candidates the writer dropped with unchanged
	// pools, whose route joins the next frame's search.
	CandidatesRechecked
	// Searches canceled by a lineage boundary (the next block, a new anchor
	// or epoch), by what the frame that brought it changed: none of the pools
	// the search was ranking through, some of them, or unknown because the
	// search was already sizing. The other three split the same searches by
	// the boundary's kind.
	SearchBoundaryUntouched
	SearchBoundaryTouched
	SearchBoundarySizing
	SearchBoundaryBlock
	SearchBoundaryAnchor
	SearchBoundaryEpoch
	counterCount
)

var counterNames = [counterCount]string{
	"messages", "receipts", "logs", "dirty_pools", "search_submitted", "search_completed",
	"search_canceled", "search_superseded", "search_canceled_overlap", "search_overtaken", "search_discarded", "paths_considered", "paths_selected",
	"candidates", "confirmation_submitted", "confirmation_completed", "confirmation_canceled",
	"confirmation_superseded", "confirmation_rejected", "plan_discarded", "ready_plans", "queue_overflow",
	"stream_invalidations", "pool_errors", "block_mismatch", "checkpoint_errors", "reconcile_errors", "search_errors", "confirmation_errors",
	"rpc_http_attempts", "rpc_method_calls", "rpc_retries",
	"explore_steps", "explore_paths", "explore_truncated", "explore_budget_exhausted", "explore_cache_full",
	"search_carried_frames", "search_path_canceled", "search_path_reused", "candidates_streamed", "candidates_duplicate",
	"plans_held",
	"reject_oracle", "reject_route", "reject_fee_estimate", "reject_fee_cap", "reject_expiry",
	"reject_simulation", "reject_economics", "reject_dispatch",
	"feed_disconnects", "feed_recoveries", "invalid_payloads",
	"rpc_http_errors", "rpc_throttled",
	"discovery_completed", "discovery_errors", "discovery_stale", "discovery_pools_added", "discovery_pools_skipped",
	"tick_repairs_deferred", "quote_out_of_window", "quote_capacity", "research_dropped",
	"reactive_admitted", "reactive_built", "reactive_sent", "reactive_rejected", "reactive_cancels",
	"reactive_landed", "reactive_succeeded", "reactive_reverted",
	"canonical_flash", "canonical_rpc", "canonical_verified", "canonical_mismatch",
	"canonical_fee_slipstream", "canonical_fee_v4", "canonical_fee_other",
	"search_frames_absorbed", "search_long_completed", "search_long_deferred", "search_long_preempted", "search_resumed", "candidates_rechecked",
	"search_boundary_untouched", "search_boundary_touched", "search_boundary_sizing", "search_boundary_block", "search_boundary_anchor", "search_boundary_epoch",
}

// Gauge values describe the current position of one source. Writers replace
// the value; queues register a sampler so reading stays off the hot path.
type Gauge uint8

const (
	HeadBlock Gauge = iota
	HeadTimestamp
	PoolCount
	FeedConnected
	FeedBacklog
	RPCCallBudget
	DeliveryPaused
	SyncLag
	GraphVersion
	GraphCycles
	GraphTruncated
	ConfirmationBacklog
	ResearchBacklog
	DiskFree
	OracleUpdated
	GraphInstalled
	// The reactive dispatcher's nonce lane and spend budget. Money is in gwei of
	// the native token, like the estimates below.
	DispatchHalted
	DispatchStopped
	DispatchNextNonce
	DispatchPending
	DispatchIncluded
	DispatchSpent
	DispatchDailyCap
	WalletNative
	// PipelineSilent is a bit set: 1 when canonical blocks stopped arriving,
	// 2 when the reactive path stopped admitting results.
	PipelineSilent
	// ExploreCatalogPaths is the background path catalog's size at the latest
	// search, observed routes included; compare with explore_paths.
	ExploreCatalogPaths
	gaugeCount
)

var gaugeNames = [gaugeCount]string{
	"head_block", "head_timestamp_seconds", "pools", "feed_connected", "feed_backlog",
	"rpc_call_budget", "delivery_paused", "sync_lag_blocks",
	"graph_version", "graph_cycles", "graph_truncated",
	"confirmation_backlog", "research_backlog",
	"disk_free_bytes", "oracle_updated_seconds", "graph_installed_seconds",
	"dispatch_halted", "dispatch_stopped", "dispatch_next_nonce", "dispatch_pending", "dispatch_included",
	"dispatch_spent_gwei", "dispatch_daily_cap_gwei", "wallet_native_gwei",
	"pipeline_silent", "explore_catalog_paths",
}

// Estimate totals describe simulated plans, never settled transactions. Money
// is in gwei of the native token: gwei keeps a uint64 from overflowing, and one
// denomination is the only way amounts from different assets can be summed.
type Estimate uint8

const (
	EstimateRoutes Estimate = iota
	EstimateGrossProfit
	EstimateNetProfit
	EstimateBudgetNetProfit
	EstimateGas
	EstimateL1Gas
	EstimateFee
	EstimateL1Fee
	EstimateL2Fee
	EstimateHeldRoutes
	EstimateHeldNetProfit
	estimateCount
)

var estimateNames = [estimateCount]string{
	"routes", "gross_profit_gwei", "net_profit_gwei", "budget_net_profit_gwei", "gas_used", "l1_gas_used",
	"fee_gwei", "l1_fee_gwei", "l2_fee_gwei", "held_routes", "held_net_profit_gwei",
}

type Stage uint8

const (
	FeedQueue Stage = iota
	PoolApply
	SearchQueue
	SearchEvaluate
	ConfirmationQueue
	ConfirmationBatch
	ReadyLatency
	ReceiptLatency
	CandidateSearch
	FirstCandidate
	RPCBlock
	RPCReceipts
	RPCLogs
	RPCCall
	RPCBatch
	RPCOther
	PlanValidity
	ReactiveRefresh
	// Feed receipt of the triggering frame to the start of the send, the send
	// call itself, and the send to our transaction appearing in the feed.
	ReactiveTriggerToSend
	ReactiveSendAck
	ReactiveSendToFeed
	// A standard feed message's arrival to its block being applied from RPC:
	// how long the local node and the follower add before search sees it.
	FeedToApply
	// The same feed message's arrival to the node announcing its block on
	// the newHeads subscription: the node's own processing time.
	FeedToHead
	// The follower's wake to its block, receipts and fee reads being ready;
	// the wait for the runtime to take the block; and its apply.
	FollowFetch
	FollowQueue
	FollowAccept
	// A standard feed message's arrival to a Fast message of the same block,
	// such as one streamed by a local Nitro node tap.
	FeedToFast
	// Parts of building a reactive intent: ranking the search result, quoting
	// smaller principals, and planning one intent (bid, calldata, seal), and
	// the whole build of one offer, including builds that admit nothing.
	ReactiveRank
	ReactiveRequote
	ReactivePlan
	ReactiveBuild
	// Parts of a completed search evaluation: cycle exploration (catalog),
	// view preparation, coarse ranking, and exact-pipeline preparation.
	SearchCatalog
	SearchPrepare
	SearchRank
	SearchExactPrepare
	// FastLaneQuote is one fast-lane evaluation on the main loop, including
	// those that find nothing.
	FastLaneQuote
	// FirstShortCandidate runs from the arrival of the oldest frame a search
	// absorbed to its first accepted candidate of at most three pools.
	// SearchShort is a completed search's evaluation up to its last
	// short-route quote; SearchLong is its long-route phase.
	FirstShortCandidate
	SearchShort
	SearchLong
	stageCount
)

var stageNames = [stageCount]string{
	"feed_queue", "pool_apply", "search_queue", "search_evaluate",
	"confirmation_queue", "confirmation_batch", "ready_latency", "receipt_latency",
	"candidate_search", "first_candidate",
	"rpc_block", "rpc_receipts", "rpc_logs", "rpc_call", "rpc_batch", "rpc_other",
	"plan_validity",
	"reactive_refresh",
	"reactive_trigger_to_send", "reactive_send_ack", "reactive_send_to_feed",
	"feed_to_apply", "feed_to_head",
	"follow_fetch", "follow_queue", "follow_accept",
	"feed_to_fast",
	"reactive_rank", "reactive_requote", "reactive_plan", "reactive_build",
	"search_catalog", "search_prepare", "search_rank", "search_exact_prepare",
	"fast_lane_quote",
	"first_short_candidate", "search_short", "search_long",
}

// Exclusive bins keep one increment per observation. Exported buckets are
// cumulative. All observations retain nanosecond precision, including zero.
var bounds = [...]time.Duration{50 * time.Microsecond, 250 * time.Microsecond, time.Millisecond,
	5 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second}

type histogram struct {
	sum  atomic.Uint64
	bins [len(bounds) + 1]atomic.Uint64
}

// Registry has constant storage and supports concurrent observers. A nil
// registry disables collection. Snapshots are pointwise atomic, not a global
// transaction across all metrics while workers are running.
type Registry struct {
	counters [sourceCount][counterCount]atomic.Uint64
	stages   [sourceCount][stageCount]histogram
	gauges   [sourceCount][gaugeCount]atomic.Int64

	estimates [sourceCount][estimateCount]atomic.Uint64
	// HTTP attempts and method calls (batch members) per RPC caller.
	rpcRequests [rpc.CallerCount]atomic.Uint64
	rpcCalls    [rpc.CallerCount]atomic.Uint64
	lastRoute   [sourceCount]atomic.Uint64

	build   Build
	started time.Time

	samplersMu sync.Mutex
	samplers   []sampler
}

type sampler struct {
	source Source
	gauge  Gauge
	read   func() int64
}

// Start and Since skip wall-clock reads when collection is disabled.
func (r *Registry) Start() time.Time {
	if r == nil {
		return time.Time{}
	}
	return time.Now()
}

func (r *Registry) Since(source Source, stage Stage, started time.Time) {
	if r != nil {
		r.Observe(source, stage, time.Since(started))
	}
}

func (r *Registry) Add(source Source, counter Counter, count uint64) {
	if r != nil && source < sourceCount && counter < counterCount {
		r.counters[source][counter].Add(count)
	}
}

func (r *Registry) Observe(source Source, stage Stage, elapsed time.Duration) {
	if r == nil || source >= sourceCount || stage >= stageCount || elapsed < 0 {
		return
	}
	h := &r.stages[source][stage]
	h.sum.Add(uint64(elapsed))
	index := len(bounds)
	for i, bound := range bounds {
		if elapsed <= bound {
			index = i
			break
		}
	}
	h.bins[index].Add(1)
}

func (r *Registry) AddEstimate(source Source, estimate Estimate, value uint64) {
	if r != nil && source < sourceCount && estimate < estimateCount {
		r.estimates[source][estimate].Add(value)
	}
}

// NewRoute reports whether route differs from the source's previous plan. An
// open opportunity is reconfirmed on every block, so summing each plan would
// count one chance again and again. Routes that alternate still count twice.
func (r *Registry) NewRoute(source Source, route uint64) bool {
	return r != nil && source < sourceCount && r.lastRoute[source].Swap(route) != route
}

func (r *Registry) Set(source Source, gauge Gauge, value int64) {
	if r != nil && source < sourceCount && gauge < gaugeCount {
		r.gauges[source][gauge].Store(value)
	}
}

// Sample evaluates read on every snapshot instead of on every queue operation.
func (r *Registry) Sample(source Source, gauge Gauge, read func() int64) {
	if r == nil || source >= sourceCount || gauge >= gaugeCount {
		return
	}
	r.samplersMu.Lock()
	defer r.samplersMu.Unlock()
	r.samplers = append(r.samplers, sampler{source, gauge, read})
}

func (r *Registry) refreshSamples() {
	r.samplersMu.Lock()
	defer r.samplersMu.Unlock()
	for _, s := range r.samplers {
		r.gauges[s.source][s.gauge].Store(s.read())
	}
}

// ConnectionObserver reports one feed connection state. It is nil when
// collection is disabled, which feed clients treat as no observer.
func (r *Registry) ConnectionObserver(source Source) func(bool) {
	if r == nil {
		return nil
	}
	return func(connected bool) {
		var value int64
		if connected {
			value = 1
		} else {
			r.Add(source, FeedDisconnects, 1)
		}
		r.Set(source, FeedConnected, value)
	}
}

// Head records the newest accepted position of one source.
func (r *Registry) Head(source Source, block, timestamp uint64) {
	r.Set(source, HeadBlock, int64(block))
	r.Set(source, HeadTimestamp, int64(timestamp))
}

type contextKey struct{}

func WithRegistry(ctx context.Context, registry *Registry) context.Context {
	return context.WithValue(ctx, contextKey{}, registry)
}

func FromContext(ctx context.Context) *Registry {
	registry, _ := ctx.Value(contextKey{}).(*Registry)
	return registry
}
