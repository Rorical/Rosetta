package arb

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// discoveryGate holds the first job inside discovery: Prepare runs on the
// discovery context after the catalog stage and before ranking.
type discoveryGate struct {
	entered, release, canceled chan struct{}
	once                       sync.Once
}

func newDiscoveryGate() *discoveryGate {
	return &discoveryGate{entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (g *discoveryGate) prepare(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
	first := false
	g.once.Do(func() { close(g.entered); first = true })
	if !first {
		return view, nil, nil
	}
	select {
	case <-g.release:
		return view, nil, nil
	case <-ctx.Done():
		close(g.canceled)
		return nil, nil, ctx.Err()
	}
}

type observations struct {
	mu   sync.Mutex
	seen []SearchObservation
}

func (o *observations) observe(observation SearchObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, observation)
}

func (o *observations) only(t *testing.T) SearchObservation {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.seen) != 1 {
		t.Fatal("search observed", len(o.seen), "times")
	}
	return o.seen[0]
}

// The fixture has two disjoint routes: A = pools[0], pools[1] and
// B = pools[2], pools[3]. The base job's Dirty lists pools[0] and pools[2].
type discoveryFrames struct {
	w             *Worker
	job           SearchJob
	gate          *discoveryGate
	first         *observations
	routeA, poolB domain.PoolKey
	routeAOther   domain.PoolKey
}

func newDiscoveryFrames(t *testing.T, triggers func(routeA, routeB domain.PoolKey) []domain.PoolKey) discoveryFrames {
	t.Helper()
	w, job, q, _ := incrementalFixture(t, false)
	close(q.finish)
	f := discoveryFrames{w: w, gate: newDiscoveryGate(), first: &observations{}, routeA: job.Dirty[0], poolB: job.Dirty[1]}
	f.routeAOther = testV2State(2, 2, 1, 1000000, 2000000).Key
	job.Dirty = triggers(f.routeA, f.poolB)
	job.Prepare = f.gate.prepare
	job.Observe = f.first.observe
	f.job = job
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, f.gate.entered)
	return f
}

func (f discoveryFrames) frame(t *testing.T, dirty ...domain.PoolKey) SearchJob {
	t.Helper()
	next := f.job
	next.Version.TxIndex++
	next.Dirty = dirty
	next.Observe = nil
	if err := f.w.Submit(next); err != nil {
		t.Fatal(err)
	}
	return next
}

// drain consumes events until the first job's final event and returns the
// first job's candidates that survive Rebase, keyed by their route.
func (f discoveryFrames) drain(t *testing.T, latest SearchJob) [][]domain.PoolKey {
	t.Helper()
	var routes [][]domain.PoolKey
	for range 16 {
		event := nextSearch(t, f.w)
		original := event.Job.Version
		valid := f.w.Rebase(&event)
		if valid && event.Job.Version != latest.Version {
			t.Fatal("carried result kept its obsolete frame version")
		}
		if valid && !f.w.Accept(event) {
			t.Fatal("current carried result was not accepted")
		}
		releaseSearch(event.Job)
		if original != f.job.Version {
			if !event.Partial {
				t.Fatal("newer frame finished before the first search: it was canceled")
			}
			continue
		}
		if !event.Partial {
			return routes
		}
		if valid {
			routes = append(routes, event.Result.Opportunities[0].Cycle.Pools)
		}
	}
	t.Fatal("first job never finished")
	return nil
}

func canceledNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func hasRoute(routes [][]domain.PoolKey, key domain.PoolKey) bool {
	return slices.ContainsFunc(routes, func(route []domain.PoolKey) bool { return slices.Contains(route, key) })
}

func TestDiscoveryOutlivesFrameDisjointFromItsDependencies(t *testing.T) {
	f := newDiscoveryFrames(t, func(routeA, _ domain.PoolKey) []domain.PoolKey { return []domain.PoolKey{routeA} })
	latest := f.frame(t, f.poolB)
	if canceledNow(f.gate.canceled) {
		t.Fatal("frame disjoint from the search's dependencies canceled discovery")
	}
	close(f.gate.release)
	routes := f.drain(t, latest)
	if !hasRoute(routes, f.routeA) {
		t.Fatal("unchanged route from the older frame was not delivered", routes)
	}
	if o := f.first.only(t); o.Outcome != SearchCompleted || !o.Overtaken || o.Overlapped {
		t.Fatal("kept search observation", o)
	}
}

func TestDiscoveryKeepsUnchangedRoutesWhenFrameTouchesOneDependency(t *testing.T) {
	f := newDiscoveryFrames(t, func(routeA, poolB domain.PoolKey) []domain.PoolKey { return []domain.PoolKey{routeA, poolB} })
	latest := f.frame(t, f.routeAOther)
	if canceledNow(f.gate.canceled) {
		t.Fatal("frame sparing a trigger pool canceled discovery")
	}
	close(f.gate.release)
	routes := f.drain(t, latest)
	if hasRoute(routes, f.routeAOther) {
		t.Fatal("route changed after the search's frame was delivered", routes)
	}
	if !hasRoute(routes, f.poolB) {
		t.Fatal("unchanged route was not delivered", routes)
	}
}

func TestDiscoveryCancelsWhenFrameChangesEveryTrigger(t *testing.T) {
	f := newDiscoveryFrames(t, func(routeA, _ domain.PoolKey) []domain.PoolKey { return []domain.PoolKey{routeA} })
	f.frame(t, f.routeA)
	waitSignal(t, f.gate.canceled)
	close(f.gate.release)
	for {
		event := nextSearch(t, f.w)
		releaseSearch(event.Job)
		if !event.Partial {
			break
		}
	}
	if o := f.first.only(t); o.Outcome != SearchCanceled || !o.Overlapped {
		t.Fatal("overlap cancellation observation", o)
	}
}

// A frame that spares the trigger cancels neither phase: the short route
// publishes at the newer frame and the long routes are ranked to the end.
// The earlier rule let the queued frame preempt the long-route rotation, and
// the preempted search counted as canceled.
func TestQueuedFrameLeavesLongRoutesRunning(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 3, 1000000, 2000000), testV2State(3, 3, 1, 1000000, 2000000), testV2State(4, 3, 4, 1000000, 2000000), testV2State(5, 4, 5, 1000000, 2000000), testV2State(6, 5, 6, 1000000, 2000000), testV2State(7, 6, 1, 1000000, 2000000)}
	short := []domain.PoolKey{pools[0].Key, pools[1].Key, pools[2].Key}
	long := []domain.PoolKey{pools[0].Key, pools[1].Key, pools[3].Key, pools[4].Key, pools[5].Key, pools[6].Key}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, long, short), 1000, 1, v2CurveQuoter())
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	gate, first := newDiscoveryGate(), &observations{}
	// The gate holds the job inside its short ranking, at the first probe.
	job := SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}, PrepareExact: gate.prepare, Observe: first.observe}
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, gate.entered)
	next := job
	next.Version.TxIndex = 1
	next.Dirty = []domain.PoolKey{pools[5].Key}
	next.Observe, next.PrepareExact = nil, nil
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	if canceledNow(gate.canceled) {
		t.Fatal("frame sparing the trigger canceled discovery before its short routes")
	}
	close(gate.release)
	carried := false
	for {
		event := nextSearch(t, w)
		original := event.Job.Version
		valid := w.Rebase(&event)
		if valid {
			w.Accept(event)
		}
		releaseSearch(event.Job)
		if original == job.Version && valid && event.Partial && slices.Equal(event.Result.Opportunities[0].Cycle.Pools, short) {
			carried = carried || event.Job.Version == next.Version
		}
		if original == job.Version && !event.Partial {
			break
		}
	}
	if !carried {
		t.Fatal("short route from the older frame was not delivered at the newer frame")
	}
	if o := first.only(t); o.Outcome != SearchCompleted || o.Long != LongCompleted || !o.Overtaken || o.Overlapped {
		t.Fatal("kept search observation", o)
	}
}
