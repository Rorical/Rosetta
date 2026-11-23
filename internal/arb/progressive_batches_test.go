package arb

import (
	"context"
	"math/big"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestProgressiveBatchesVisitEveryPathOnceAndResumeCursor(t *testing.T) {
	engine, _, _, _ := recordedSearchFixture(t)
	// Only immutable lengths are needed by the batching scheduler here.
	layout := &compiledPaths{paths: make([]graph.PackedPath, 13001)}
	for i := range layout.paths {
		layout.paths[i].Length = uint32(2 + i%5)
	}
	engine.compiled = layout
	indices := make([]int, len(layout.paths))
	for i := range indices {
		indices[i] = i
	}
	worker := &Worker{cpus: newSearchCPUs()}
	var cache rankCache
	for i := range cache.progressCursor {
		cache.progressCursor[i] = 103
	}
	for range 2 {
		seen := make([]int, len(indices))
		depths := []int{}
		lease, err := worker.cpus.acquire(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		job := SearchJob{initials: &progressQuotes{}}
		catalogCalled := false
		schedule := worker.progressiveSchedule(t.Context(), &job, engine, &cache, &lease, time.Now(), rankPass{afterFirst: func() error {
			if !slices.Equal(depths, []int{2, 3}) || worker.cpus.held() != 0 {
				t.Fatal("supplemental catalog waited for broad scan or held its CPU permit", depths)
			}
			catalogCalled = true
			return nil
		}})
		_, err = schedule(indices, 1, func(part []int, _ int) (RankedSelection, error) {
			if len(part) > rankBatchSize {
				t.Error("unbounded batch", len(part))
			}
			depth := int(layout.paths[part[0]].Length)
			depths = append(depths, depth)
			for _, index := range part {
				seen[index]++
				if int(layout.paths[index].Length) != depth {
					t.Error("mixed depths")
				}
			}
			return RankedSelection{considered: len(part)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !catalogCalled || !slices.Equal(depths[:5], []int{2, 3, 2, 3, 4}) {
			t.Fatal("lost short priority or rotation", depths)
		}
		for index, count := range seen {
			if count != 1 {
				t.Fatal("missing or repeated path", index, count)
			}
		}
		if worker.cpus.held() != 0 {
			t.Fatal("leaked CPU permit")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	lease, err := worker.cpus.acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	job := SearchJob{initials: &progressQuotes{}}
	calls := 0
	_, err = worker.progressiveSchedule(ctx, &job, engine, &cache, &lease, time.Now(), rankPass{})(indices, 1, func(part []int, _ int) (RankedSelection, error) { calls++; cancel(); return RankedSelection{}, nil })
	if err != context.Canceled || calls != 1 || worker.cpus.held() != 0 {
		t.Fatal("preemption failed to yield CPU", err, calls)
	}
}

func TestProgressiveReductionPreservesDiverseReservoirs(t *testing.T) {
	engine, _, _, _ := recordedSearchFixture(t)
	const count = rankBatchSize*3 + 17
	engine.compiled = &compiledPaths{paths: make([]graph.PackedPath, count)}
	indices := make([]int, count)
	for i := range indices {
		indices[i] = i
		engine.compiled.paths[i].Length = uint32(2 + i%5)
	}
	rank := func(part []int, limit int) (RankedSelection, error) {
		bucket := rankBucket{diverse: true}
		for _, index := range part {
			key := domain.PoolKey{Address: domain.Address{byte(index%43 + 1)}, Kind: domain.PoolKindV2}
			// Unique identities exercise the top-K plus best-per-pool reservoirs.
			depth := 2 + index%5
			cycle := graph.Cycle{Start: domain.Address{1}, Pools: make([]domain.PoolKey, depth), Len: depth}
			cycle.Pools[0] = key
			for hop := 1; hop < depth-1; hop++ {
				cycle.Pools[hop] = domain.PoolKey{Address: domain.Address{0xfe, byte(hop)}, Kind: domain.PoolKindV2}
			}
			cycle.Pools[depth-1] = domain.PoolKey{Address: domain.Address{byte(index >> 8), byte(index)}, Kind: domain.PoolKindV2}
			bucket.admit(rankedCycle{cycle: cycle, net: big.NewInt(int64(index + 1)), input: big.NewInt(1)}, limit, false)
		}
		return RankedSelection{candidates: bucket.members(), considered: len(part)}, nil
	}
	want, err := rankInShards(indices, engine.K(), 3, true, rank)
	if err != nil {
		t.Fatal(err)
	}
	worker := &Worker{cpus: newSearchCPUs(), opts: RankedOptions{Diverse: true}}
	job := SearchJob{initials: &progressQuotes{}}
	// Exhaust the provisional quote budget. This test isolates full reduction.
	for range engine.K() {
		job.initials.record(rankedCycle{cycle: graph.Cycle{Start: domain.Address{1}}})
	}
	var cache rankCache
	cache.progressCursor[2] = 901
	lease, err := worker.cpus.acquire(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := worker.progressiveSchedule(t.Context(), &job, engine, &cache, &lease, time.Now(), rankPass{})(indices, 3, rank)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("batching dropped diverse representatives or changed K", err)
	}
}

func TestSmallRankBatchLeavesUnusedCoresForExactWork(t *testing.T) {
	engine, _, _, _ := recordedSearchFixture(t)
	engine.compiled = &compiledPaths{paths: []graph.PackedPath{{Length: 2}}}
	worker := &Worker{cpus: newSearchCPUsOf(8)}
	lease, err := worker.cpus.acquire(t.Context(), 8)
	if err != nil {
		t.Fatal(err)
	}
	job := SearchJob{initials: &progressQuotes{}}
	var cache rankCache
	_, err = worker.progressiveSchedule(t.Context(), &job, engine, &cache, &lease, time.Now(), rankPass{})([]int{0}, 8, func(_ []int, _ int) (RankedSelection, error) {
		if held := worker.cpus.held(); held != 1 {
			t.Errorf("one ranking shard reserved %d CPU slots", held)
		}
		return RankedSelection{}, nil
	})
	if err != nil || worker.cpus.held() != 0 {
		t.Fatal("rank lease not released", err)
	}
}
