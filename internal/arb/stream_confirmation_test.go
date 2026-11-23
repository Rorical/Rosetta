package arb_test

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

type gatedBroadTable struct {
	table            *graph.Table
	armed            *atomic.Bool
	ctx              context.Context
	entered, release chan struct{}
	once             sync.Once
}

func (g *gatedBroadTable) Len() int { return g.table.Len() }
func (g *gatedBroadTable) Candidates(keys []domain.PoolKey, limit int) []int {
	return g.table.Candidates(keys, limit)
}
func (g *gatedBroadTable) Cycle(index int) graph.Cycle {
	cycle := g.table.Cycle(index)
	if g.armed.Load() && len(cycle.Pools) > 3 {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-g.ctx.Done():
		}
	}
	return cycle
}

type armingQuote struct {
	inner arb.Quoter
	armed *atomic.Bool
}

func (q armingQuote) QuoteExactIn(ctx context.Context, p pool.State, deep pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	q.armed.Store(true)
	return q.inner.QuoteExactIn(ctx, p, deep, token, amount)
}

type streamedConfirmation struct{ started chan struct{} }

func (t *streamedConfirmation) Run(context.Context) (*execution.ReadyPlan, []string, error) {
	close(t.started)
	return nil, nil, nil
}
func (*streamedConfirmation) Close() {}

// Hold the broad scan after its first usable route. Full sizing and confirmation
// scheduling must progress while the remainder of that scan cannot complete.
func TestStreamedSizingStartsConfirmationBeforeBroadScanFinishes(t *testing.T) {
	for _, source := range []domain.StateSource{domain.SourceStandard, domain.SourceFast} {
		t.Run(map[domain.StateSource]string{domain.SourceStandard: "standard", domain.SourceFast: "fast"}[source], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			store := state.New()
			var pools []pool.State
			pairs := [][2]byte{{1, 2}, {2, 3}, {3, 1}, {3, 4}, {4, 5}, {5, 6}, {6, 1}}
			for index, pair := range pairs {
				p := pool.State{Key: domain.PoolKey{Address: domain.Address{byte(index + 1)}, Kind: domain.PoolKindV2}, Token0: domain.Address{pair[0]}, Token1: domain.Address{pair[1]}, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000), Fee: 3000}
				pools = append(pools, p)
				if err := store.Upsert(p); err != nil {
					t.Fatal(err)
				}
			}
			short := graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key, pools[2].Key}, Len: 3}
			long := graph.Cycle{Start: short.Start, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key, pools[3].Key, pools[4].Key, pools[5].Key, pools[6].Key}, Len: 6}
			table, err := graph.BuildTableFromCycles([]graph.Cycle{long, short})
			if err != nil {
				t.Fatal(err)
			}
			var armed atomic.Bool
			gate := &gatedBroadTable{table: table, armed: &armed, ctx: ctx, entered: make(chan struct{}), release: make(chan struct{})}
			quote, err := simulate.NewQuoter(dex.NewAdapter)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := arb.New(gate, func(domain.Address) *big.Int { return big.NewInt(1000) }, 1, armingQuote{quote, &armed})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := arb.NewWorker(ctx, engine, arb.RankedOptions{StreamInitial: true, Diverse: true, SwapFee: dex.SwapFee, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
				return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == short.Start
			}})
			if err != nil {
				t.Fatal(err)
			}
			confirmations := pipeline.NewConfirmationWorker(ctx, time.Second)
			t.Cleanup(func() { cancel(); <-worker.Done(); <-confirmations.Done() })
			version := domain.StateVersion{Source: source, Epoch: 1, BlockNumber: 1}
			if err := worker.Submit(arb.SearchJob{Incremental: true, View: store.View(), PrepareExact: func(_ context.Context, v arb.View, _ []domain.PoolKey) (arb.View, []arb.QuoteIssue, error) {
				armed.Store(true)
				return v, nil, nil
			}, Dirty: []domain.PoolKey{pools[0].Key}, Version: version, GraphVersion: 1}); err != nil {
				t.Fatal(err)
			}
			next := func() arb.SearchEvent {
				t.Helper()
				select {
				case event := <-worker.Results():
					return event
				case <-time.After(2 * time.Second):
					t.Fatal("sizing waited for broad scan")
					return arb.SearchEvent{}
				}
			}
			initial := next()
			defer initial.Job.Release()
			optimized := next()
			defer optimized.Job.Release()
			if initial.Stage != arb.QuoteInitial || optimized.Stage != arb.QuoteOptimized || !optimized.Partial || len(optimized.Result.Opportunities[0].Cycle.Pools) != 3 {
				t.Fatal("not a fully sized early route", initial.Stage, optimized.Stage)
			}
			if !worker.Accept(initial) || !worker.Accept(optimized) {
				t.Fatal("early events lost their version proof")
			}
			task := &streamedConfirmation{started: make(chan struct{})}
			if accepted, err := confirmations.Offer(pipeline.ConfirmationJob{Identity: pipeline.ConfirmationIdentity{Version: optimized.Job.Version, Graph: optimized.Job.GraphVersion}, Task: task}, 1); err != nil || !accepted {
				t.Fatal("early confirmation was not offered", err)
			}
			select {
			case <-task.started:
			case <-time.After(time.Second):
				t.Fatal("confirmation waited for search final")
			}
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("broad remainder did not reach its gate")
			}
			close(gate.release)
		})
	}
}
