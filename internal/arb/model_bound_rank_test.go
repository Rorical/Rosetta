package arb

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// Disable only the new numerical ceiling to retain an independent reference
// for admission, including equal profits and depth/pool representatives.
func TestProfitCeilingPreservesRecordedRankingsAndDiverseReservoirs(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	var buffer graph.CandidateBuffer
	indices := buffer.Select(engine.table, dirty, engine.table.Len())
	compiled := new(compiledSnapshot)
	compatible, err := compiled.prepare(t.Context(), engine.compiled, indices, view, opts, 1)
	if err != nil || !compatible {
		t.Fatal(compatible, err)
	}
	generic := &rankSnapshot{pools: make(map[domain.PoolKey]*rankPool), funding: make(map[domain.Address]rankFunding)}
	if err := generic.prepare(t.Context(), engine.table, indices, view, opts); err != nil {
		t.Fatal(err)
	}
	for _, diverse := range []bool{false, true} {
		for _, limit := range []int{1, 3, 32} {
			for _, workers := range []int{1, 3} {
				compareCeilingRankings(t, engine, compiled, generic, indices, limit, workers, diverse)
			}
		}
	}
}

func compareCeilingRankings(t *testing.T, engine *Engine, compiled *compiledSnapshot, generic *rankSnapshot, indices []int, limit, workers int, diverse bool) {
	t.Helper()
	var reference RankedSelection
	for _, bounded := range []bool{false, true} {
		for i := range compiled.pools {
			setCeilingAvailability(&compiled.pools[i], bounded)
		}
		for _, p := range generic.pools {
			setCeilingAvailability(p, bounded)
		}
		got, err := rankInShards(indices, limit, workers, diverse, func(part []int, limit int) (RankedSelection, error) {
			return engine.rankCompiled(t.Context(), compiled, part, limit, nil, diverse)
		})
		if err != nil {
			t.Fatal(err)
		}
		want, err := engine.rankPrepared(t.Context(), generic, indices, limit, workers, diverse)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
		if !bounded {
			reference = got
			continue
		}
		assertSameRanking(t, reference, got)
	}
}

func setCeilingAvailability(p *rankPool, bounded bool) {
	for direction := range 2 {
		leg := &p.legs[direction]
		leg.interval = modelInterval{}
		if bounded {
			leg.interval = newModelInterval(leg.model)
		}
	}
}
