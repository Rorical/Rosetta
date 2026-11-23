package arb

import (
	"context"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type countingRankView struct {
	View
	reads map[domain.PoolKey]int
}

func (v *countingRankView) Pool(key domain.PoolKey) (pool.State, bool) {
	v.reads[key]++
	return v.View.Pool(key)
}

func TestRankShardsMatchSerialWithMultipleFundingAssetsAndIssues(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1_000_000, 2_000_000), testV2State(2, 1, 2, 1_000_000, 1_500_000),
		testV2State(3, 2, 3, 1_000_000, 2_000_000), testV2State(4, 3, 1, 1_000_000, 2_000_000),
		testV3State(5, 1, 3, 3000, 60, 1_000_000), testV2State(6, 1, 3, 1_000_000, 0),
	}
	table := mustTable(t, 3, states...)
	engine := mustEngine(t, table, 1000, 2, identityQuoter())
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, p := range states {
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	counted := &countingRankView{View: view, reads: make(map[domain.PoolKey]int)}
	opts := rankedOptions(states[0].Token0, 100000)
	opts.Bounds = func(token domain.Address) (InputBounds, bool) {
		bounds, _ := rankedOptions(token, 100000).Bounds(token)
		return bounds, token == states[0].Token0 || token == states[0].Token1
	}
	indices := table.Candidates(dirty, table.Len())
	snapshot := rankSnapshots.Get().(*rankSnapshot)
	defer snapshot.release()
	if err := snapshot.prepare(t.Context(), table, indices, counted, opts); err != nil {
		t.Fatal(err)
	}
	serial, err := engine.rankPrepared(t.Context(), snapshot, indices, engine.K(), 1, false)
	if err != nil || len(serial.candidates) != 4 || len(serial.issues) == 0 {
		t.Fatal("missing positive candidates or diagnostics", serial, err)
	}
	for _, workers := range []int{2, 4, 8} {
		for range 10 {
			parallel, err := engine.rankPrepared(t.Context(), snapshot, indices, engine.K(), workers, false)
			if err != nil || !reflect.DeepEqual(serial, parallel) {
				t.Fatalf("%d shards changed the result: %#v %#v %v", workers, serial, parallel, err)
			}
		}
	}
	for key, reads := range counted.reads {
		if reads != 1 {
			t.Fatal("pool copied more than once", key, reads)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := engine.rankPrepared(canceled, snapshot, indices, engine.K(), 4, false); err != context.Canceled {
		t.Fatal("canceled shards returned success", err)
	}
}
