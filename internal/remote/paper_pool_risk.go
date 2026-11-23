package remote

import (
	"encoding/json"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

// observePools feeds verified discovery-state outcomes to the pool
// quarantine. Unverified, rejected or later-prefix samples say nothing about
// the pools. A revert on a route holding an excluded token is the token's,
// already recorded by the transfer-shortfall registry.
func (p *executionPaper) observePools(job paperExecutionJob, result paperExecutionResult) error {
	if p.pools == nil || result.StateBasis != "discovery_snapshot" || (result.Status != "success" && result.Status != "reverted") || result.Version.Anchor.Hash == (domain.Hash{}) {
		return nil
	}
	cycle := job.opportunity.Cycle
	reverted := result.Status == "reverted"
	if reverted {
		for _, key := range cycle.Pools {
			state, ok := job.event.Job.View.Pool(key)
			if !ok || pipeline.CheckPoolTokens(state, p.risks.Check) != nil {
				return nil
			}
		}
	}
	identity, _ := json.Marshal([]any{cycle.Start, cycle.Pools})
	for _, evidence := range p.poolOutcomes.Observe(digest(identity), cycle.Pools, reverted, result.Version.Anchor) {
		if err := p.pools.Record(evidence); err != nil {
			return err
		}
	}
	return nil
}
