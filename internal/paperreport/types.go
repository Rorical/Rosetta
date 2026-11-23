// Package paperreport summarizes persisted executor simulations without RPC.
package paperreport

import (
	"encoding/json"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

type sample struct {
	Selection        *Selection           `json:"selection"`
	Bidding          *bidSample           `json:"bidding"`
	SamplingBuckets  []SamplingBucket     `json:"buckets"`
	SamplingOverflow uint64               `json:"overflow_observations"`
	RPCCalls         *uint64              `json:"rpc_calls"`
	SnapshotNS       int64                `json:"snapshot_ns"`
	SetupNS          int64                `json:"setup_ns"`
	FeePrefetchNS    int64                `json:"fee_prefetch_ns"`
	ExecutionNS      int64                `json:"execution_ns"`
	PlannedDelaysMS  []int64              `json:"planned_delays_ms"`
	QueuedAtNS       int64                `json:"queued_at_ns"`
	Type             string               `json:"type"`
	Schema           int                  `json:"schema"`
	Basis            string               `json:"basis"`
	Chain            domain.ChainIdentity `json:"chain"`
	Artifact         domain.Hash          `json:"creation_code_hash"`
	ID               domain.Hash          `json:"sample_id"`
	Group            domain.Hash          `json:"group_id"`
	DelayMS          int64                `json:"delay_ms"`
	ActualDelayNS    int64                `json:"actual_delay_ns"`
	ObservedAtNS     int64                `json:"observed_at_ns"`
	DiscoveredAtNS   int64                `json:"discovered_at_ns"`
	StartedAtNS      int64                `json:"started_at_ns"`
	FinishedAtNS     int64                `json:"finished_at_ns"`
	StateBasis       string               `json:"state_basis"`
	Status           string               `json:"status"`
	Version          json.RawMessage      `json:"version"`
	Call             json.RawMessage      `json:"call"`
	Candidate        *struct {
		Cycle struct{ Start domain.Address }
	} `json:"candidate"`
	QuotedGross *big.Int `json:"quoted_gross"`
	Profit      *big.Int `json:"profit_after_loan"`
	LoanFee     *big.Int `json:"loan_fee"`
	GasUsed     uint64   `json:"gas_used"`
	Economics   *struct {
		Basis  string   `json:"basis"`
		Status string   `json:"net_status"`
		Net    *big.Int `json:"estimated_net"`
	} `json:"economics"`
	Skipped uint64 `json:"skipped"`
}

// Amounts remain in the start token's raw units. No cross-token profit sum is
// meaningful, and a simulation is never counted as realized trading revenue.
type Report struct {
	Sampling         []StratumReport      `json:"sampling_strata"`
	Bids             []BidScenarioCounts  `json:"bid_scenarios"`
	SamplingOverflow uint64               `json:"sampling_overflow_observations"`
	Basis            string               `json:"basis"`
	Chain            domain.ChainIdentity `json:"chain"`
	Artifact         domain.Hash          `json:"creation_code_hash"`
	Samples          int                  `json:"samples"`
	Duplicates       int                  `json:"duplicate_samples"`
	Skipped          uint64               `json:"busy_candidates_skipped"`
	TornTail         bool                 `json:"incomplete_tail_ignored"`
	Buckets          []Bucket             `json:"buckets"`
}

type Bucket struct {
	SamplingWait     Distribution   `json:"sampling_wait_ns"`
	RPCCalls         Distribution   `json:"rpc_calls"`
	Snapshot         Distribution   `json:"snapshot_ns"`
	Setup            Distribution   `json:"setup_ns"`
	FeePrefetch      Distribution   `json:"fee_prefetch_ns"`
	Simulation       Distribution   `json:"execution_ns"`
	DelayMS          int64          `json:"delay_ms"`
	Asset            string         `json:"asset"`
	Samples          int            `json:"samples"`
	Status           map[string]int `json:"execution_status"`
	NetStatus        map[string]int `json:"net_status"`
	Verified         int            `json:"verified_state_samples"`
	KnownNet         int            `json:"known_net_samples"`
	NetSum           *big.Int       `json:"simulated_net_sum_raw_units"`
	QuoteComparisons int            `json:"quote_comparisons"`
	GrossErrorSum    *big.Int       `json:"actual_minus_quoted_gross_sum_raw_units"`
	Gas              Distribution   `json:"measured_gas"`
	ActualDelay      Distribution   `json:"observed_delay_ns"`
	Queue            Distribution   `json:"worker_queue_ns"`
	Execution        Distribution   `json:"worker_duration_ns"`
	Paired           PairCounts     `json:"paired_with_discovery"`
}

type Distribution struct {
	Count int    `json:"count"`
	P50   uint64 `json:"p50"`
	P95   uint64 `json:"p95"`
	Max   uint64 `json:"max"`
}

type PairCounts struct {
	MissingArrival      int `json:"missing_arrival"`
	MissingBaseline     int `json:"missing_baseline"`
	BaselineNotPositive int `json:"baseline_unknown_or_non_positive"`
	PositiveBaseline    int `json:"positive_baseline"`
	PositiveLater       int `json:"positive_later"`
	NonPositiveLater    int `json:"non_positive_later"`
	Reverted            int `json:"reverted"`
	Unknown             int `json:"unverified_or_unknown"`
	Censored            int `json:"censored"`
	MismatchedCall      int `json:"mismatched_transaction"`
}
