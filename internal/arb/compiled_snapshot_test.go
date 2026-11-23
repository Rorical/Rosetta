package arb

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Preparing the needed pools on several cores fills the snapshot exactly as
// one core does, funding included.
func TestReadNeededOnSeveralCoresMatchesOneCore(t *testing.T) {
	var states []pool.State
	for i := range 200 {
		a, b := byte(1+i%40), byte(1+(i*7+3)%40)
		if a == b {
			b = a%40 + 1
		}
		states = append(states, testV2State(byte(i+1), a, b, int64(1_000_000+i*997), int64(2_000_000+i*1301)))
	}
	packed, err := graph.PackTable(t.Context(), mustTable(t, 3, states...), states)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, packed, 1000, 4, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, s := range states {
		view.add(s)
		dirty = append(dirty, s.Key)
	}
	opts := rankedOptions(states[0].Token0, 100000)
	var serial, parallel compiledSnapshot
	okSerial, err := serial.prepareDirty(t.Context(), engine.compiled, dirty, view, opts, 1)
	if err != nil || !okSerial {
		t.Fatal(okSerial, err)
	}
	okParallel, err := parallel.prepareDirty(t.Context(), engine.compiled, dirty, view, opts, 8)
	if err != nil || !okParallel {
		t.Fatal(okParallel, err)
	}
	if len(parallel.ids) < 2*parallelSnapshotPools {
		t.Fatal("fixture does not exercise several workers", len(parallel.ids))
	}
	if !reflect.DeepEqual(serial.pools, parallel.pools) || !reflect.DeepEqual(serial.funding, parallel.funding) || !reflect.DeepEqual(serial.tokens, parallel.tokens) {
		t.Fatal("parallel preparation differs from the serial one")
	}
}

// Updating the rank cache on several cores leaves every pool revision, path
// input and rotation mask that a serial update does, frame after frame.
func TestRankCacheUpdateOnSeveralCoresMatchesOneCore(t *testing.T) {
	var states []pool.State
	for i := range 200 {
		a, b := byte(1+i%40), byte(1+(i*7+3)%40)
		if a == b {
			b = a%40 + 1
		}
		states = append(states, testV2State(byte(i+1), a, b, int64(1_000_000+i*997), int64(2_000_000+i*1301)))
	}
	packed, err := graph.PackTable(t.Context(), mustTable(t, 3, states...), states)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, packed, 1000, 4, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	var dirty []domain.PoolKey
	for _, s := range states {
		dirty = append(dirty, s.Key)
	}
	base := rankedOptions(states[0].Token0, 100000)
	errFixtureFee := errors.New("fixture fee unavailable")
	failing := base
	failing.SwapFee = func(p pool.State, token domain.Address) (uint32, error) {
		if p.Key == states[3].Key && token == p.Token0 {
			return 0, errFixtureFee
		}
		return base.SwapFee(p, token)
	}
	frame := func(step int) (*fakeView, RankedOptions) {
		view := newFakeView()
		for i, s := range states {
			switch {
			case step == 1 && i%3 == 0:
				s.Reserve0 = big.NewInt(int64(1_100_000 + i))
			case step == 1 && i >= 5 && i < 10:
				continue // absent this frame
			}
			view.add(s)
		}
		if step == 1 {
			return view, failing
		}
		return view, base
	}
	var serial, parallel rankCache
	serial.bind(engine.compiled, 1)
	parallel.bind(engine.compiled, 1)
	for step := range 3 {
		view, opts := frame(step)
		for _, cache := range []struct {
			c       *rankCache
			workers int
		}{{&serial, 1}, {&parallel, 8}} {
			var snapshot compiledSnapshot
			if ok, err := snapshot.prepareDirty(t.Context(), engine.compiled, dirty, view, opts, 1); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := cache.c.updateWorkers(t.Context(), &snapshot, cache.workers, 0); err != nil {
				t.Fatal(err)
			}
		}
		if serial.revision != parallel.revision || !reflect.DeepEqual(serial.inputs, parallel.inputs) || !reflect.DeepEqual(serial.rotations, parallel.rotations) {
			t.Fatalf("step %d: parallel cache update differs from the serial one", step)
		}
		for id := range serial.pools {
			if serial.pools[id].revision != parallel.pools[id].revision {
				t.Fatalf("step %d: pool %d revision differs", step, id)
			}
		}
	}
	if serial.revision < 3 {
		t.Fatal("fixture changed too little", serial.revision)
	}
}
