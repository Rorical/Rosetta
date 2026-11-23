package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type tickTask struct {
	ctx        context.Context
	reader     *snapshot
	state      pool.State
	radius     int16
	generation uint64
}
type tickAnswer struct {
	task tickTask
	deep pool.DeepState
	err  error
}

// One canonical acquisition survives advancing heads. Its completed window is
// replayed through retained receipts before adoption; Fast state is never read.
type tickRepair struct {
	jobs       chan tickTask
	answers    chan tickAnswer
	done       chan struct{}
	cancel     context.CancelFunc
	cancelTask context.CancelFunc
	busy       bool
	generation uint64
	demand     map[domain.PoolKey]int16
	retryAt    map[domain.PoolKey]uint64
	tail       []canonicalUpdate
	bytes      int
}

func newTickRepair(ctx context.Context) *tickRepair {
	ctx, cancel := context.WithCancel(ctx)
	d := &tickRepair{jobs: make(chan tickTask, 1), answers: make(chan tickAnswer, 1), done: make(chan struct{}), cancel: cancel, demand: make(map[domain.PoolKey]int16), retryAt: make(map[domain.PoolKey]uint64)}
	go func() {
		defer close(d.done)
		for {
			select {
			case <-ctx.Done():
				return
			case task := <-d.jobs:
				deep, err := loadRepairTicks(task)
				task.reader.Close()
				select {
				case d.answers <- tickAnswer{task: task, deep: deep, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return d
}
func (d *tickRepair) close() {
	if d.cancelTask != nil {
		d.cancelTask()
	}
	d.cancel()
	<-d.done
}
func loadRepairTicks(task tickTask) (pool.DeepState, error) {
	tag := fmt.Sprintf("0x%x", task.reader.block.Ref.Number)
	for {
		var deep pool.DeepState
		var err error
		switch task.state.Key.Kind {
		case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindSolidlyV3:
			deep, err = (&uniswapv3.DeepLoader{Caller: task.reader}).LoadDeep(task.ctx, task.state, tag, task.radius)
		case domain.PoolKindCamelotV3:
			deep, err = camelotv3.LoadDeep(task.ctx, task.reader, task.state, tag, task.radius)
		case domain.PoolKindV4:
			deep, err = uniswapv4.LoadDeep(task.ctx, task.reader, task.state, tag, task.radius)
		default:
			return deep, fmt.Errorf("unsupported tick repair kind %d", task.state.Key.Kind)
		}
		if err == nil {
			deep.LoadedBlock = task.reader.block.Ref
			return deep, nil
		}
		if task.ctx.Err() != nil || !rpc.IsRateLimited(err) {
			return deep, err
		}
		// The hash-bound cache retains successful chunks; only throttled misses retry.
	}
}

func (d *tickRepair) schedule(ctx context.Context, r *runtime) {
	if d == nil || d.busy || r.catchingUp && !r.nearTip {
		return
	}
	// Half the target is the reload hysteresis: a fresh window covers the full
	// target, so ordinary drift does not reload it every few ticks.
	for _, p := range r.store.Pools() {
		if !p.Key.Kind.UsesTicks() {
			continue
		}
		low, high, _, ok := r.store.DeepStatus(p.Key)
		if !ok || !pool.WindowCovers(p.Tick, low, high, math.CoverageTicks/2) {
			d.demand[p.Key] = r.windowRadius(p)
		}
	}
	var keys []domain.PoolKey
	for key := range d.demand {
		if d.retryAt[key] <= r.store.Block().Number {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	for _, key := range keys {
		p, ok := r.store.Pool(key)
		if !ok {
			delete(d.demand, key)
			continue
		}
		jobCtx, cancel := context.WithTimeout(rpc.WithCaller(ctx, rpc.CallerTick), 5*time.Minute)
		d.cancelTask = cancel
		d.busy = true
		d.tail = nil
		d.bytes = 0
		d.jobs <- tickTask{ctx: jobCtx, reader: r.reader(), state: p, radius: d.demand[key], generation: d.generation}
		return
	}
}

func (d *tickRepair) observe(update canonicalUpdate) {
	if d == nil {
		return
	}
	if update.reset != nil {
		d.generation++
		d.tail = nil
		d.bytes = 0
		if d.cancelTask != nil {
			d.cancelTask()
		}
		return
	}
	if !d.busy {
		return
	}
	size := 0
	for _, tx := range update.transactions {
		for _, log := range tx.Logs {
			size += len(log.Data) + 32*len(log.Topics) + 96
		}
	}
	d.tail = append(d.tail, update)
	d.bytes += size
	if len(d.tail) > 1024 || d.bytes > 32<<20 {
		d.tail = nil
		d.bytes = 0
		d.generation++
		d.cancelTask()
	}
}

func (d *tickRepair) consume(ctx context.Context, r *runtime, answer tickAnswer) error {
	d.busy = false
	if d.cancelTask != nil {
		d.cancelTask()
		d.cancelTask = nil
	}
	if answer.task.generation != d.generation {
		return nil
	}
	key := answer.task.state.Key
	if answer.err != nil {
		d.retryAt[key] = r.store.Block().Number + 4
		slog.Warn("canonical tick repair deferred", "pool", key.Address.Hex(), "error", answer.err)
		r.metrics.Add(telemetry.Standard, telemetry.TickRepairsDeferred, 1)
		return nil
	}
	deep, ok, err := rebaseRepair(ctx, r.store.View(), answer, d.tail)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err = r.store.SetDeep(key, deep); err != nil {
		return err
	}
	r.store.ClearDirty()
	if err = r.capture.Dependencies(r.store.Block(), nil, []state.DeepDelta{{Key: key, State: deep}}); err != nil {
		return err
	}
	delete(d.demand, key)
	delete(d.retryAt, key)
	slog.Info("canonical tick window repaired", "pool", key.Address.Hex(), "block", r.store.Block().Number, "ticks", len(deep.Ticks), "radius", answer.task.radius)
	if err = r.refreshTentativeDependencies(ctx, []domain.PoolKey{key}); err != nil {
		return err
	}
	return r.submit(ctx, r.store.View(), []domain.PoolKey{key}, r.version)
}

// request queues a reload for a quote that lacked ticks. Every load uses the
// coverage radius around the current tick. A window exit is ignored once the
// canonical window covers the target again: it came from an older view, and
// exits past a covered side are capacity, which never reaches this queue.
func (d *tickRepair) request(r *runtime, issue arb.QuoteIssue) {
	missing := errors.Is(issue.Err, arb.ErrMissingDeep)
	if !missing && !errors.Is(issue.Err, math.ErrOutOfWindow) {
		return
	}
	p, ok := r.store.Pool(issue.Pool)
	if !ok || !p.Key.Kind.UsesTicks() {
		return
	}
	if !missing {
		low, high, _, loaded := r.store.DeepStatus(issue.Pool)
		if loaded && pool.WindowCovers(p.Tick, low, high, math.CoverageTicks) {
			return
		}
	}
	d.demand[issue.Pool] = r.windowRadius(p)
}

// windowRadius is the bitmap radius of every canonical tick load for p.
func (r *runtime) windowRadius(p pool.State) int16 {
	return pool.WindowRadius(p.TickSpacing, r.policy.WordRadius)
}
