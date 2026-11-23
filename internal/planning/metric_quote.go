package planning

import (
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
)

// metricQuoteMaxAge is the oldest oracle quote a route through a MetricOmm
// pool is built on. The pool's price provider reverts once the block time is
// more than 10,000 ms past the quote's own timestamp, or more than 1,000 ms
// before it (PriceProviderFailed, read from the deployed contracts), and the
// oracle is typically updated about every 2 s. Half the window leaves the
// other half for the block time's whole
// seconds, the intent's lifetime (at most 2 s) and inclusion; a quote that
// missed two updates is also no longer the market's.
const metricQuoteMaxAge = 5 * time.Second

// metricQuoteMaxLead mirrors the provider's lower bound: a quote stamped
// further ahead of the block is refused on chain.
const metricQuoteMaxLead = time.Second

// checkMetricQuotes refuses a route through a MetricOmm pool whose state is
// stale or whose quote is missing or outside the age window at now, and
// every such route when the executor is not known to have the leg.
func checkMetricQuotes(view arb.View, o arb.Opportunity, supported bool, now time.Time) error {
	for _, key := range o.Cycle.Pools {
		state, ok := view.Pool(key)
		if !ok || state.Metric == nil {
			continue
		}
		if !supported {
			return fmt.Errorf("metric leg not enabled for the executor")
		}
		metric := state.Metric
		if metric.Stale || metric.BidX64 == nil || metric.AskX64 == nil {
			return fmt.Errorf("metric quote unavailable")
		}
		age := now.UnixMilli() - int64(metric.QuoteMS)
		if age > metricQuoteMaxAge.Milliseconds() || age <= -metricQuoteMaxLead.Milliseconds() {
			return fmt.Errorf("metric quote outside its age window")
		}
	}
	return nil
}
