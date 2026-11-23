package arb

import (
	"context"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// overlayView replaces a few pools of the recorded prefix.
type overlayView struct {
	base  View
	pools map[domain.PoolKey]pool.State
}

func (v overlayView) Pool(key domain.PoolKey) (pool.State, bool) {
	if state, ok := v.pools[key]; ok {
		return state, true
	}
	return v.base.Pool(key)
}
func (v overlayView) Deep(key domain.PoolKey) (pool.DeepState, bool) { return v.base.Deep(key) }

// Frames arrive faster than a search finishes, as 200 ms flashblocks do
// against 216 ms median evaluation. Each frame moves the next reserve-based
// pool of the recorded OP prefix, so consecutive frames have different trigger
// pools and heavily shared dependencies. "catalog" adds a discovery delay
// before ranking in the production proportion (140 ms catalog per 200 ms
// frame); the recorded fixture has no catalog stage of its own. Run with
// -benchtime=Nx: one op is one frame. A frame is covered once the writer
// accepts an optimized candidate through its trigger pool quoted at that frame
// or later; covered-ms is the mean time from the frame to that candidate.
func BenchmarkFrameCadence(b *testing.B) {
	const interval = 2 * time.Millisecond
	for _, mode := range []struct {
		name    string
		catalog time.Duration
	}{{"fixture", 0}, {"catalog", interval * 7 / 10}} {
		b.Run(mode.name, func(b *testing.B) { frameCadence(b, interval, mode.catalog) })
	}
}

type cadenceFrame struct {
	trigger       domain.PoolKey
	at, coveredAt time.Time
}

func frameCadence(b *testing.B, interval, catalog time.Duration) {
	fixture := newFrameFixture(b)
	ctx, cancel := context.WithCancel(b.Context())
	w, err := NewWorker(ctx, fixture.engine, fixture.opts)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { cancel(); <-w.Done() }()
	var mu sync.Mutex
	outcomes := map[SearchOutcome]int{}
	overtaken, overlapped := 0, 0
	var queue time.Duration
	var frames []cadenceFrame
	observe := func(o SearchObservation) {
		mu.Lock()
		defer mu.Unlock()
		outcomes[o.Outcome]++
		queue += o.QueueWait
		switch {
		case o.Overlapped:
			overlapped++
		case o.Overtaken:
			overtaken++
		}
	}
	cover := func(origin uint64, route []domain.PoolKey, at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		for index := range min(int(origin), len(frames)) {
			frame := &frames[index]
			if frame.coveredAt.IsZero() && slices.Contains(route, frame.trigger) {
				frame.coveredAt = at
			}
		}
	}
	prepare := func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
		if catalog == 0 {
			return view, nil, nil
		}
		select {
		case <-time.After(catalog):
			return view, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-w.Results():
				origin := event.Job.Version.TxIndex
				if w.Rebase(&event) && w.Accept(event) && event.Partial && event.Stage == QuoteOptimized {
					cover(origin, event.Result.Opportunities[0].Cycle.Pools, time.Now())
				}
				releaseSearch(event.Job)
			}
		}
	}()
	pools := map[domain.PoolKey]pool.State{}
	frame := uint64(0)
	b.ResetTimer()
	for b.Loop() {
		frame++
		key := fixture.reserveSet[int(frame)%len(fixture.reserveSet)]
		state, ok := pools[key]
		if !ok {
			state, _ = fixture.base.Pool(key)
		}
		reserve := new(big.Int).Mul(state.Reserve0, big.NewInt(1001))
		state.Reserve0 = reserve.Quo(reserve, big.NewInt(1000))
		next := make(map[domain.PoolKey]pool.State, len(pools)+1)
		for k, v := range pools {
			next[k] = v
		}
		next[key] = state
		pools = next
		job := SearchJob{Incremental: true, View: overlayView{base: fixture.base, pools: pools}, Dirty: []domain.PoolKey{key}, GraphVersion: 1, ReceivedAt: time.Now(), Observe: observe, Prepare: prepare,
			Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 1, TxIndex: frame}}
		mu.Lock()
		frames = append(frames, cadenceFrame{trigger: key, at: job.ReceivedAt})
		mu.Unlock()
		if err := w.Submit(job); err != nil {
			b.Fatal(err)
		}
		time.Sleep(interval)
	}
	b.StopTimer()
	time.Sleep(200 * time.Millisecond) // let the last search finish
	cancel()
	<-consumed
	mu.Lock()
	defer mu.Unlock()
	count := float64(b.N)
	covered, latency := 0, time.Duration(0)
	for _, f := range frames {
		if !f.coveredAt.IsZero() {
			covered++
			latency += f.coveredAt.Sub(f.at)
		}
	}
	b.ReportMetric(float64(outcomes[SearchCompleted])/count, "completed/frame")
	b.ReportMetric(float64(outcomes[SearchCanceled])/count, "canceled/frame")
	b.ReportMetric(float64(outcomes[SearchSuperseded])/count, "superseded/frame")
	b.ReportMetric(float64(overtaken)/count, "overtaken/frame")
	b.ReportMetric(float64(overlapped)/count, "overlapped/frame")
	b.ReportMetric(float64(covered)/count, "covered/frame")
	if covered > 0 {
		b.ReportMetric(float64(latency.Microseconds())/float64(covered)/1000, "covered-ms")
	}
	if runs := outcomes[SearchCompleted] + outcomes[SearchCanceled] + outcomes[SearchSuperseded]; runs > 0 {
		b.ReportMetric(float64((queue/time.Duration(runs)).Microseconds())/1000, "queue-ms")
	}
}
