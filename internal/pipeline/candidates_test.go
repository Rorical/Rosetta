package pipeline

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"testing"
)

func TestCandidateAdmissionAppliesFundingBeforeCapAndDeduplicatesRetryPools(t *testing.T) {
	first, second := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	opportunities := []arb.Opportunity{
		{Cycle: graph.Cycle{Start: domain.Address{9}, Pools: []domain.PoolKey{first}}},
		{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{second, first}}},
		{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{first, second}}},
	}
	selected, retry := SelectCandidates(opportunities, 1, func(asset domain.Address) bool { return asset == (domain.Address{1}) })
	if len(selected) != 1 || selected[0].Cycle.Start != (domain.Address{1}) || len(retry) != 2 || retry[0] != first || retry[1] != second {
		t.Fatal("unfunded ranking consumed cap or retry set differs", selected, retry)
	}
}
