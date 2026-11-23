package arb

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestDiverseAdmissionPreservesProfitLeadersAndNewPools(t *testing.T) {
	bucket := rankBucket{diverse: true}
	asset := domain.Address{1}
	for i := 0; i < 24; i++ {
		// Many high-ranked alternatives share three pools; the low-ranked short
		// cycle introduces an independent market and must retain an exact slot.
		keys := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}, {Address: domain.Address{3}}, {Address: domain.Address{byte(i + 10)}}}
		bucket.admit(rankedCycle{cycle: graph.Cycle{Start: asset, Pools: keys, Len: 4}, net: big.NewInt(int64(100 - i))}, 8, false)
	}
	short := rankedCycle{cycle: graph.Cycle{Start: asset, Pools: []domain.PoolKey{{Address: domain.Address{90}}, {Address: domain.Address{91}}}, Len: 2}, net: big.NewInt(1)}
	bucket.admit(short, 8, false)
	selected := bucket.selectCandidates(8)
	if len(selected) != 8 {
		t.Fatal("candidate cap changed", len(selected))
	}
	found := false
	for i, c := range selected {
		if i < 4 && c.net.Int64() != int64(100-i) {
			t.Fatal("strongest half lost")
		}
		found = found || sameRankedRoute(c, short)
	}
	if !found {
		t.Fatal("unrepresented short market lost to repeated variants")
	}
}
func TestRecordedDiverseRankingMatchesAcrossShardCounts(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	opts.Diverse = true
	indices := engine.table.Candidates(dirty, engine.table.Len())
	snapshot := compiledSnapshots.Get().(*compiledSnapshot)
	defer snapshot.release()
	if compatible, err := snapshot.prepare(t.Context(), engine.compiled, indices, view, opts, 1); err != nil || !compatible {
		t.Fatal("recorded compiled snapshot", err)
	}
	rank := func(workers int) (RankedSelection, error) {
		return rankInShards(indices, engine.K(), workers, true, func(part []int, limit int) (RankedSelection, error) {
			return engine.rankCompiled(t.Context(), snapshot, part, limit, nil, true)
		})
	}
	serial, err := rank(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{2, 4} {
		actual, err := rank(workers)
		if err != nil || !reflect.DeepEqual(actual, serial) {
			t.Fatal("diverse reduction depends on shard count", workers, err)
		}
	}
	byAsset := map[domain.Address]int{}
	for _, c := range serial.candidates {
		byAsset[c.cycle.Start]++
	}
	for _, n := range byAsset {
		if n > engine.K() {
			t.Fatal("diverse exact cap exceeded")
		}
	}
}

func TestDiverseReservoirReductionMatchesSerialWhenCandidatesExceedK(t *testing.T) {
	candidates := make([]rankedCycle, 96)
	indices := make([]int, len(candidates))
	for i := range candidates {
		indices[i] = i
		keys := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}, {Address: domain.Address{byte(i%32 + 10)}}}
		if i%7 == 0 {
			keys = keys[:2]
		}
		candidates[i] = rankedCycle{cycle: graph.Cycle{Start: domain.Address{1}, Pools: keys, Len: len(keys)}, net: big.NewInt(int64(1000 - i)), path: i}
	}
	rank := func(workers int) (RankedSelection, error) {
		return rankInShards(indices, 8, workers, true, func(part []int, limit int) (RankedSelection, error) {
			bucket := rankBucket{diverse: true}
			for _, index := range part {
				bucket.admit(candidates[index], limit, false)
			}
			return RankedSelection{candidates: bucket.members(), considered: len(part)}, nil
		})
	}
	serial, err := rank(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{2, 4, 8} {
		got, err := rank(workers)
		if err != nil || !reflect.DeepEqual(got, serial) {
			t.Fatal("reservoir lost a shard representative", workers, err)
		}
	}
}
