package arb

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/graph"
)

// Grouping on several cores keeps every group in index order, as one core does.
func TestCompiledDepthsMatchesSerialGrouping(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	paths := make([]graph.PackedPath, 5000)
	for i := range paths {
		paths[i].Length = uint32(2 + random.IntN(18)) // past 16 shares the last group
	}
	for trial := range 50 {
		var indices []int
		for i := range paths {
			if random.IntN(3) == 0 {
				indices = append(indices, i)
			}
		}
		var want [17][]int
		for _, index := range indices {
			depth := min(16, int(paths[index].Length))
			want[depth] = append(want[depth], index)
		}
		workers := 2 + trial%7
		got := compiledDepths(paths, indices, make([]int, len(indices)), workers)
		for depth := range want {
			if !slices.Equal(want[depth], got[depth]) {
				t.Fatalf("trial %d, %d workers: depth %d differs", trial, workers, depth)
			}
		}
	}
}

// Batches ranked ahead on several cores commit in plan order, and a failed
// batch stops the commits right before it.
func TestRankAheadCommitsInPlanOrder(t *testing.T) {
	w := &Worker{cpus: newSearchCPUs()}
	plan := make([]rankBatch, 60)
	for i := range plan {
		plan[i] = rankBatch{depth: 2 + i%3, cursor: i, paths: []int{i}}
	}
	failAt := map[bool]int{false: -1, true: 37}
	for _, failing := range []bool{false, true} {
		random := rand.New(rand.NewPCG(9, uint64(failAt[failing]+1)))
		delays := make([]time.Duration, len(plan))
		for i := range delays {
			delays[i] = time.Duration(random.IntN(2000)) * time.Microsecond
		}
		broken := errors.New("rank failed")
		rank := func(paths []int, _ int) (RankedSelection, error) {
			time.Sleep(delays[paths[0]])
			if paths[0] == failAt[failing] {
				return RankedSelection{}, broken
			}
			return RankedSelection{considered: 1}, nil
		}
		var committed []int
		err := w.rankAhead(t.Context(), &SearchJob{}, plan, 8, 4, cpuNormal, rank, func(batch rankBatch, part RankedSelection) error {
			if part.considered != 1 {
				t.Fatal("commit received another batch's result")
			}
			committed = append(committed, batch.cursor)
			return nil
		})
		want := len(plan)
		if failing {
			want = failAt[true]
			if !errors.Is(err, broken) {
				t.Fatal("failure not returned", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if len(committed) != want {
			t.Fatalf("committed %d batches, want %d", len(committed), want)
		}
		for i, cursor := range committed {
			if cursor != i {
				t.Fatalf("batch %d committed in position %d", cursor, i)
			}
		}
	}
	if w.cpus.held() != 0 {
		t.Fatal("rankAhead kept CPU slots", w.cpus.held())
	}
}
