package arb

import "time"

// SearchStages separates elapsed waits and work. Durations encode nanoseconds.
// Per-quote path fields are independent; sums across parallel paths are not wall time.
type SearchStages struct {
	Intake           time.Duration `json:"input_to_submit_ns"`
	Queue            time.Duration `json:"queue_ns"`
	Catalog          time.Duration `json:"catalog_ns"`
	Prepare          time.Duration `json:"prepare_ns"`
	RankCPU          time.Duration `json:"rank_cpu_wait_ns"`
	Rank             time.Duration `json:"rank_ns"`
	ExactPrepare     time.Duration `json:"exact_prepare_ns"`
	Snapshot         time.Duration `json:"snapshot_ns"`
	PathQueue        time.Duration `json:"path_queue_ns"`
	PathCPU          time.Duration `json:"path_cpu_wait_ns"`
	Math             time.Duration `json:"quote_math_ns"`
	Provisional      time.Duration `json:"provisional_quote_ns"`
	PriorPublication time.Duration `json:"prior_publication_ns"`
	// Short runs from the start of evaluation until every short-route quote
	// of the job is published or dropped; Long is the long-route phase after it.
	Short time.Duration `json:"short_ns"`
	Long  time.Duration `json:"long_ns"`
}
