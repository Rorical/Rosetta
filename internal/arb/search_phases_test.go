package arb

import (
	"context"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// phaseMarket has, per index i, a two-pool route (entry i, exit i) and a
// four-pool route (entry i and three pools of its own), all from token 1.
// Every pool starts balanced, so nothing is profitable until a frame moves an
// entry pool; a quote through entry i therefore proves a search at or after
// the frame that moved it.
type phaseMarket struct {
	start domain.Address
	entry []pool.State
	short [][]domain.PoolKey
	long  [][]domain.PoolKey
	view  *fakeView
}

func newPhaseMarket(count int) phaseMarket {
	m := phaseMarket{start: testAddress(1), view: newFakeView()}
	for i := range count {
		id, token := byte(10*(i+1)), byte(20+i)
		entry := testV2State(id, 1, token, 1000000, 1000000)
		exit := testV2State(id+1, token, 1, 1000000, 1000000)
		second := testV2State(id+2, token, 100+byte(i), 1000000, 1000000)
		third := testV2State(id+3, 100+byte(i), 140+byte(i), 1000000, 1000000)
		fourth := testV2State(id+4, 140+byte(i), 1, 1000000, 1000000)
		for _, p := range []pool.State{entry, exit, second, third, fourth} {
			m.view.add(p)
		}
		m.entry = append(m.entry, entry)
		m.short = append(m.short, []domain.PoolKey{entry.Key, exit.Key})
		m.long = append(m.long, []domain.PoolKey{entry.Key, second.Key, third.Key, fourth.Key})
	}
	return m
}

// moved returns a view in which entries [0, upTo] pay two for one.
func (m phaseMarket) moved(upTo int) *fakeView {
	view := newFakeView()
	for _, state := range m.view.states {
		view.add(state)
	}
	for _, entry := range m.entry[:upTo+1] {
		entry.Reserve1 = big.NewInt(2000000)
		view.add(entry)
	}
	return view
}

func (m phaseMarket) worker(t *testing.T, quoter Quoter) *Worker {
	t.Helper()
	// K covers every route, so a search that absorbs several frames drops none.
	engine := mustEngine(t, explicitTable(t, m.start, slices.Concat(m.short, m.long)...), 1000, 32, quoter)
	opts := rankedOptions(m.start, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	return w
}

func (m phaseMarket) frame(index int, dirty ...domain.PoolKey) SearchJob {
	return SearchJob{Incremental: true, View: m.moved(index), Dirty: dirty, GraphVersion: 1,
		Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10, TxIndex: uint64(index)}}
}

// stepGate holds each job inside its short ranking, at the first exact
// preparation, until the test releases it, while scripted.
type stepGate struct {
	entered, release chan struct{}
	free             atomic.Bool
}

func newStepGate() *stepGate {
	return &stepGate{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

// hold returns one job's PrepareExact.
func (g *stepGate) hold() func(context.Context, View, []domain.PoolKey) (View, []QuoteIssue, error) {
	var once sync.Once
	return func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
		first := false
		once.Do(func() { first = true })
		if !first || g.free.Load() {
			return view, nil, nil
		}
		g.entered <- struct{}{}
		select {
		case <-g.release:
			return view, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// holdQuoter blocks the first quote through key until release, or reports
// the cancellation of that quote.
type holdQuoter struct {
	key                        domain.PoolKey
	entered, release, canceled chan struct{}
	once                       sync.Once
}

func newHoldQuoter(key domain.PoolKey) *holdQuoter {
	return &holdQuoter{key: key, entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (q *holdQuoter) QuoteExactIn(ctx context.Context, p pool.State, _ pool.DeepState, token domain.Address, input *big.Int) (*big.Int, error) {
	if p.Key == q.key {
		held := false
		q.once.Do(func() { close(q.entered); held = true })
		if held {
			select {
			case <-q.release:
			case <-ctx.Done():
				close(q.canceled)
				return nil, ctx.Err()
			}
		}
	}
	in, out, _ := traversalReserves(p, token)
	return math.QuoteV2ExactIn(in, out, p.Fee, input)
}

// delivered collects, on its own goroutine, the candidates that pass the
// writer's dependency check, and every final event.
type delivered struct {
	mu     sync.Mutex
	routes []deliveredRoute
	finals []domain.StateVersion
}

type deliveredRoute struct {
	pools  []domain.PoolKey
	origin domain.StateVersion
}

func (d *delivered) consume(w *Worker) {
	for event := range w.Results() {
		origin := event.Job.Version
		valid := w.Rebase(&event) && w.Accept(event)
		d.mu.Lock()
		if !event.Partial {
			d.finals = append(d.finals, origin)
		} else if valid && event.Stage == QuoteOptimized {
			d.routes = append(d.routes, deliveredRoute{event.Result.Opportunities[0].Cycle.Pools, origin})
		}
		d.mu.Unlock()
		releaseSearch(event.Job)
	}
}

func (d *delivered) has(route []domain.PoolKey) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.ContainsFunc(d.routes, func(r deliveredRoute) bool { return slices.Equal(r.pools, route) })
}

func (d *delivered) finished(version domain.StateVersion) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.finals, version)
}

// eventually polls a condition another goroutine will make true.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Every search is still ranking its short routes when the next frame, on
// another pool, arrives. None is canceled, and each pool's short and long route is quoted
// at a state that includes its own frame. Under the earlier rule the queued
// frame preempted every search but the last after its first short batches.
func TestFramesFasterThanSearchesStillCompleteAndCoverEveryPool(t *testing.T) {
	const frames = 6
	market := newPhaseMarket(frames)
	w := market.worker(t, v2CurveQuoter())
	gate, seen, results := newStepGate(), &observations{}, &delivered{}
	go results.consume(w)
	submit := func(index int) {
		job := market.frame(index, market.entry[index].Key)
		job.PrepareExact, job.Observe = gate.hold(), seen.observe
		if err := w.Submit(job); err != nil {
			t.Fatal(err)
		}
	}
	submit(0)
	for index := range frames {
		waitSignal(t, gate.entered)
		if index+1 < frames {
			submit(index + 1)
		}
		gate.release <- struct{}{}
	}
	gate.free.Store(true)
	for index := range frames {
		eventually(t, "short route of every frame", func() bool { return results.has(market.short[index]) })
		eventually(t, "long route of every frame", func() bool { return results.has(market.long[index]) })
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	completed := 0
	for _, o := range seen.seen {
		switch o.Outcome {
		case SearchCanceled:
			t.Fatal("a frame on another pool canceled a search", o)
		case SearchCompleted:
			completed++
		}
	}
	if completed < frames {
		t.Fatal("searches completed", completed, "of", frames)
	}
}

// The first search is held inside its long route's sizing. A later frame's
// short route is ranked, sized and delivered, and that search ends, without
// waiting for it: the lane used to run one search at a time.
func TestLaterShortRoutesDoNotWaitForEarlierLongRoutes(t *testing.T) {
	market := newPhaseMarket(2)
	quoter := newHoldQuoter(market.long[0][1])
	w := market.worker(t, quoter)
	results, second := &delivered{}, &observations{}
	go results.consume(w)
	if err := w.Submit(market.frame(0, market.entry[0].Key)); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, quoter.entered)
	next := market.frame(1, market.entry[1].Key)
	next.Observe = second.observe
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the later frame's short route", func() bool { return results.has(market.short[1]) })
	eventually(t, "the later frame's search", func() bool { return results.finished(next.Version) })
	if o := second.only(t); o.Outcome != SearchCompleted || o.Long != LongDeferred || o.Frames != 1 {
		t.Fatal("later search observation", o)
	}
	close(quoter.release)
	// Its long route was left owed and is ranked once the first search ends.
	eventually(t, "the later frame's deferred long route", func() bool { return results.has(market.long[1]) })
}

// A frame changes one pool of the route a running search is sizing and one
// pool of a route whose quote is already queued for the writer. Neither quote
// may pass: the first is canceled, the second fails its dependency proof.
func TestChangedDependencyNeverPublishesTheOldQuote(t *testing.T) {
	market := newPhaseMarket(1)
	quoter := newHoldQuoter(market.long[0][1])
	w := market.worker(t, quoter)
	if err := w.Submit(market.frame(0, market.entry[0].Key)); err != nil {
		t.Fatal(err)
	}
	// The short route's quotes wait unread in the result queue.
	waitSignal(t, quoter.entered)
	next := market.frame(0, market.short[0][1], market.long[0][2])
	next.Version.TxIndex = 1
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, quoter.canceled)
	results := &delivered{}
	go results.consume(w)
	eventually(t, "the newer frame's search", func() bool { return results.finished(next.Version) })
	results.mu.Lock()
	defer results.mu.Unlock()
	for _, route := range results.routes {
		if route.origin != next.Version {
			t.Fatal("quote on a changed pool's old state was published", route.pools)
		}
	}
}

// A block, an invalidated lineage and a replaced graph each cancel both
// running searches: the one sizing long routes and the one ranking short ones.
func TestBoundariesCancelBothRunningSearches(t *testing.T) {
	boundaries := map[string]func(*testing.T, *Worker, phaseMarket){
		"block": func(t *testing.T, w *Worker, market phaseMarket) {
			job := market.frame(1)
			job.Version.BlockNumber++
			if err := w.Submit(job); err != nil {
				t.Fatal(err)
			}
		},
		"invalidate": func(_ *testing.T, w *Worker, _ phaseMarket) { w.Invalidate(domain.SourceFast) },
		"graph": func(t *testing.T, w *Worker, market phaseMarket) {
			engine := mustEngine(t, explicitTable(t, market.start, market.short...), 1000, 32, v2CurveQuoter())
			if err := w.ReplaceEngine(engine, 2); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, boundary := range boundaries {
		t.Run(name, func(t *testing.T) {
			market := newPhaseMarket(2)
			quoter := newHoldQuoter(market.long[0][1])
			w := market.worker(t, quoter)
			go (&delivered{}).consume(w)
			if err := w.Submit(market.frame(0, market.entry[0].Key)); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, quoter.entered)
			gate := newDiscoveryGate()
			next := market.frame(1, market.entry[1].Key)
			next.PrepareExact = gate.prepare
			if err := w.Submit(next); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, gate.entered)
			boundary(t, w, market)
			waitSignal(t, quoter.canceled)
			waitSignal(t, gate.canceled)
		})
	}
}

// A block boundary cancels a search whose short routes were ranked and
// acknowledged but not yet sized. The next block searches them again.
func TestBoundaryRepeatsShortRoutesThatWereNotSized(t *testing.T) {
	market := newPhaseMarket(1)
	quoter := newHoldQuoter(market.short[0][1])
	w := market.worker(t, quoter)
	results := &delivered{}
	go results.consume(w)
	if err := w.Submit(market.frame(0, market.entry[0].Key)); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, quoter.entered)
	eventually(t, "the short routes' acknowledgement", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.owed[domain.SourceFast].short) == 0
	})
	block := market.frame(0)
	block.Version.BlockNumber++
	if err := w.Submit(block); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, quoter.canceled)
	eventually(t, "the short route in the next block", func() bool {
		results.mu.Lock()
		defer results.mu.Unlock()
		return slices.ContainsFunc(results.routes, func(r deliveredRoute) bool {
			return r.origin == block.Version && slices.Equal(r.pools, market.short[0])
		})
	})
}

// Long-route work never takes the last core of four or more, and none that
// queued short-route work is about to ask for.
func TestNormalClassLeavesCoresForShortRoutes(t *testing.T) {
	cpus := newSearchCPUsOf(4)
	long, err := cpus.acquire(t.Context(), 4)
	if err != nil || long.count != 3 {
		t.Fatal("normal class took", long.count, err)
	}
	short, err := cpus.acquireClass(t.Context(), 1, cpuUrgent)
	if err != nil || short.count != 1 {
		t.Fatal("urgent class could not take the reserved core", err)
	}
	long.release()
	cpus.expect(3)
	if _, ok := cpus.tryAcquire(); ok {
		t.Fatal("normal class took a core that queued short-route work expects")
	}
	granted := make(chan searchCPULease, 1)
	go func() {
		lease, _ := cpus.acquire(t.Context(), 1)
		granted <- lease
	}()
	eventually(t, "the normal waiter", func() bool {
		cpus.mu.Lock()
		defer cpus.mu.Unlock()
		return len(cpus.waiting[cpuNormal]) == 1
	})
	cpus.met(3)
	lease := <-granted
	lease.release()
	short.release()
	if small := newSearchCPUsOf(2); small.normalLimit != 2 {
		t.Fatal("two-core host reserved a core from long routes")
	}
	if cpus.held() != 0 || cpus.normal != 0 || cpus.expected != 0 {
		t.Fatal("leaked CPU accounting", cpus.held(), cpus.normal, cpus.expected)
	}
}

// A freed core goes to the newest search's short routes, then to earlier
// searches' short sizing, then to long routes, whatever order they queued in.
func TestFreedCoreGoesToNewestShortWorkFirst(t *testing.T) {
	cpus := newSearchCPUsOf(1)
	held, err := cpus.acquireClass(t.Context(), 1, cpuUrgent)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan cpuClass, 3)
	for _, class := range []cpuClass{cpuNormal, cpuBacklog, cpuUrgent} {
		go func() {
			lease, _ := cpus.acquireClass(t.Context(), 1, class)
			order <- class
			lease.release()
		}()
		eventually(t, "the waiter", func() bool {
			cpus.mu.Lock()
			defer cpus.mu.Unlock()
			return len(cpus.waiting[class]) == 1
		})
	}
	held.release()
	for _, want := range []cpuClass{cpuUrgent, cpuBacklog, cpuNormal} {
		if got := <-order; got != want {
			t.Fatal("core granted to class", got, "before", want)
		}
	}
}
