package arb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdmath "math"
	"math/big"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// InputBounds caps search to amounts the funding source can actually supply.
// Floor, when set, is the smallest gross worth ranking or publishing: below it
// a route cannot pay even the cheapest executor transaction.
type InputBounds struct{ Min, Max, Floor *big.Int }

// clearsFloor reports whether gross is positive and reaches the floor.
func (b InputBounds) clearsFloor(gross *big.Int) bool {
	return gross != nil && gross.Sign() > 0 && (b.Floor == nil || gross.Cmp(b.Floor) >= 0)
}

// publishable applies the funding asset's floor to an exact quote.
func (o RankedOptions) publishable(quote Opportunity) bool {
	bounds, _ := o.Bounds(quote.Cycle.Start)
	return bounds.clearsFloor(quote.Net)
}

type RankedOptions struct {
	// StreamInitial publishes a complete positive quote before sizing finishes.
	StreamInitial bool
	// Diverse reserves exact-quote slots for distinct depths and pool coverage.
	Diverse bool
	// ExactK caps, per funding asset and search job, how many ranked
	// candidates a Worker sizes with the full amount search. It applies only
	// when 0 < ExactK < K; zero sizes every selected candidate. Streamed
	// candidates past the cap keep their one-amount initial quote.
	ExactK int
	// ReservedCPUs is how many of GOMAXPROCS a Worker leaves alone, for a
	// caller that quotes on its own goroutine while the Worker searches. The
	// Worker always keeps one.
	ReservedCPUs int
	// Bounds returns false for assets unavailable as the route's funding token.
	Bounds func(domain.Address) (InputBounds, bool)
	// SwapFee supplies directional protocol fees without coupling arb to dex.
	SwapFee func(pool.State, domain.Address) (uint32, error)
	// Model supplies protocol-specific screening reserves. Nil retains the
	// original constant-product/concentrated-liquidity model for old callers.
	Model func(pool.State) (pool.RankingModel, error)
}

type SearchResult struct {
	ExactCanceled, ExactReused int
	Published                  int
	Exploration                ExplorationStats
	Opportunities              []Opportunity
	Issues                     []QuoteIssue
	Considered                 int
	Selected                   int
	// SelectedRoutes keys (RouteKey) the routes a fast-lane engine's coarse
	// rank selected for exact quotes; other engines leave it empty.
	SelectedRoutes []string

	// ProbeOnly counts selected paths that ExactK left at their initial quote.
	ProbeOnly int
	// Limits counts sizing samples rejected at the edge of modeled liquidity.
	Limits QuoteLimits
}

// QuoteLimits separates samples that need more tick data from samples larger
// than a pool can fill. Only the first kind is also reported as a QuoteIssue.
type QuoteLimits struct {
	OutOfWindow int
	Capacity    int
}

func (l *QuoteLimits) add(other QuoteLimits) {
	l.OutOfWindow += other.OutOfWindow
	l.Capacity += other.Capacity
}

type rankedCycle struct {
	cycle          graph.Cycle
	bounds         InputBounds
	input          *big.Int
	net            *big.Int
	path, rotation int
	revision       uint64
}

// EvaluateRanked performs both stages for callers whose exact dependencies are
// already available. Workers use Rank and Optimize to load only selected ticks.
func (e *Engine) EvaluateRanked(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions) (SearchResult, error) {
	selection, err := e.Rank(ctx, view, dirty, opts)
	if err != nil {
		return SearchResult{}, err
	}
	result, err := e.Optimize(ctx, view, selection)
	if _, fast := e.table.(*shortCycles); fast {
		for _, candidate := range selection.candidates {
			result.SelectedRoutes = append(result.SelectedRoutes, routeKey(candidate.cycle))
		}
	}
	result.Opportunities = slices.DeleteFunc(result.Opportunities, func(o Opportunity) bool { return !opts.publishable(o) })
	return result, err
}

// Rank evaluates only paths in the dirty-pool index. It retains K estimates per
// profit asset, so different token denominations are never compared.
func (e *Engine) Rank(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions) (RankedSelection, error) {
	return e.rank(ctx, view, dirty, opts, nil)
}

func (e *Engine) rank(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions, cache *rankCache) (RankedSelection, error) {
	return e.rankWithWorkers(ctx, view, dirty, opts, cache, runtime.GOMAXPROCS(0))
}

func (e *Engine) rankWithWorkers(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions, cache *rankCache, workers int) (RankedSelection, error) {
	return e.rankScheduled(ctx, view, dirty, opts, cache, workers, nil)
}

// rankShardSize is the fewest candidates that earn another ranking worker.
// At 1024, mid-size sets run serially while an 8-core host's cores idle.
// Reduction stays deterministic for any shard count.
const rankShardSize = 256

// smallHostRankShardSize keeps the earlier sizing on two cores or fewer: there
// a second ranking worker takes the core the main loop's fast lane needs.
const smallHostRankShardSize = 1024

// rankSchedule owns reduction when ranking is split into progressive batches.
type rankSchedule func([]int, int, func([]int, int) (RankedSelection, error)) (RankedSelection, error)

func (e *Engine) rankScheduled(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions, cache *rankCache, workers int, schedule rankSchedule) (RankedSelection, error) {
	return e.rankPass(ctx, view, dirty, opts, cache, workers, schedule, true)
}

// rankPass is rankScheduled for one pass of a search. Only a search's first
// pass is counted: phantom pools sit out a number of rankings, one per search.
func (e *Engine) rankPass(ctx context.Context, view View, dirty []domain.PoolKey, opts RankedOptions, cache *rankCache, workers int, schedule rankSchedule, counted bool) (RankedSelection, error) {
	if schedule == nil {
		schedule = func(indices []int, workers int, rank func([]int, int) (RankedSelection, error)) (RankedSelection, error) {
			return rankInShards(indices, e.K(), workers, opts.Diverse, rank)
		}
	}
	selection := RankedSelection{}
	if err := ctx.Err(); err != nil {
		return selection, err
	}
	if counted {
		e.phantoms.beginRanking()
	}
	if view == nil || opts.Bounds == nil || opts.SwapFee == nil {
		return selection, fmt.Errorf("%w: ranked search requires view, funding bounds and swap fees", ErrInvalidConfig)
	}
	work := rankWorkspaces.Get().(*rankWorkspace)
	defer work.release()
	indices := work.candidates.SelectWorkers(e.table, dirty, e.table.Len(), workers)
	shard := rankShardSize
	if workers <= 2 {
		shard = smallHostRankShardSize
	}
	workers = min(max(1, workers), max(1, (len(indices)+shard-1)/shard))
	if e.compiled != nil {
		snapshot := compiledSnapshots.Get().(*compiledSnapshot)
		defer snapshot.release()
		var compatible bool
		var err error
		if table, ok := e.table.(*graph.Table); ok && table.PackedLayout() != nil {
			compatible, err = snapshot.prepareDirty(ctx, e.compiled, dirty, view, opts, workers)
		} else {
			compatible, err = snapshot.prepare(ctx, e.compiled, indices, view, opts, workers)
		}
		if err != nil {
			return selection, err
		}
		if compatible {
			if cache != nil {
				if err := cache.update(ctx, snapshot, workers); err != nil {
					return selection, err
				}
			}
			selection, err := schedule(indices, workers, func(part []int, limit int) (RankedSelection, error) {
				return e.rankCompiled(ctx, snapshot, part, limit, cache, opts.Diverse)
			})
			return selection, err
		}
	}
	snapshot := rankSnapshots.Get().(*rankSnapshot)
	defer snapshot.release()
	if err := snapshot.prepare(ctx, e.table, indices, view, opts); err != nil {
		return selection, err
	}
	// Two source lanes share the CPU budget. Sets under rankShardSize stay
	// serial; larger ones use contiguous shards with deterministic top-K
	// reduction.
	return schedule(indices, workers, func(part []int, limit int) (RankedSelection, error) {
		return e.rankIndices(ctx, snapshot, part, limit, opts.Diverse)
	})
}

func (e *Engine) rankPrepared(ctx context.Context, snapshot *rankSnapshot, indices []int, limit, workers int, diverse bool) (RankedSelection, error) {
	return rankInShards(indices, limit, workers, diverse, func(part []int, limit int) (RankedSelection, error) {
		return e.rankIndices(ctx, snapshot, part, limit, diverse)
	})
}

func (e *Engine) rankIndices(ctx context.Context, snapshot *rankSnapshot, indices []int, limit int, diverse bool) (RankedSelection, error) {
	selection := RankedSelection{}
	ranked := make(map[domain.Address]*rankBucket)
	work := rankWorkspaces.Get().(*rankWorkspace)
	defer work.release()

	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return selection, err
		}
		cycle := work.cycle(e.table, index)
		cycles, err := work.fundedCycles(snapshot, cycle)
		if err != nil {
			return selection, err
		}
		selection.considered += len(cycles)
		if len(cycles) == 0 || !work.cycleCanProfit(snapshot, cycle) {
			continue
		}
		for _, rotation := range cycles {
			candidate := work.rotate(cycle, rotation)
			bucket := ranked[candidate.cycle.Start]
			upper := work.genericUpper(snapshot, candidate.cycle, rotation.low, rotation.high)
			if !stdmath.IsInf(upper, 1) {
				ceilFloat(&work.upper, upper)
				if !candidate.bounds.clearsFloor(&work.upper) || (bucket != nil && bucket.rejectPathUpper(&work.upper, candidate.cycle, limit)) {
					continue
				}
			}
			input, net, err := work.estimateRankedCycle(snapshot, candidate.cycle, candidate.bounds)
			if err != nil {
				var issue *QuoteIssue
				if errors.As(err, &issue) {
					selection.issues = appendIssue(selection.issues, *issue)
				}
				continue
			}
			candidate.input, candidate.net = input, net
			if candidate.bounds.clearsFloor(net) && !e.phantoms.excludes(candidate.cycle.Pools) {
				bucket = ranked[candidate.cycle.Start]
				if bucket == nil {
					bucket = &rankBucket{diverse: diverse}
					ranked[candidate.cycle.Start] = bucket
				}
				bucket.admit(candidate, limit, true)
			}
		}
	}
	for _, bucket := range ranked {
		selection.candidates = append(selection.candidates, bucket.members()...)
	}
	return selection, nil
}

func (w *rankWorkspace) cycleCanProfit(snapshot *rankSnapshot, cycle graph.Cycle) bool {
	w.legs = slices.Grow(w.legs[:0], len(cycle.Pools))[:len(cycle.Pools)]
	current := cycle.Start
	rateUpper, rateLower := 1.0, 1.0
	approximate := false
	for i, key := range cycle.Pools {
		leg, next, err := snapshot.leg(key, current)
		if err != nil {
			// Preserve the funded rotations' original dependency diagnostics.
			return true
		}
		w.legs[i], current = leg.model, next
		approximate = approximate || leg.approximate
		rateUpper = stdmath.Nextafter(rateUpper*leg.rateUpper, stdmath.Inf(1))
		rateLower = roundDown(rateLower * leg.rateLower)
	}
	if approximate {
		return true
	}
	return marginProfitable(rateUpper, rateLower, w.legs)
}

// Optimize performs exact amount search only on the bounded selection, against
// the same snapshot used by Rank with any selected dependencies now populated.
func (e *Engine) Optimize(ctx context.Context, view View, selection RankedSelection) (SearchResult, error) {
	result := SearchResult{Considered: selection.considered, Issues: slices.Clone(selection.issues)}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Candidates are sized in parallel, each worker with its own quote view,
	// then reduced in selection order, so the result is the serial one.
	type sized struct {
		opportunity Opportunity
		issues      []QuoteIssue
		ok          bool
		err         error
		limits      QuoteLimits
	}
	candidates := selection.candidates
	outcomes := make([]sized, len(candidates))
	var next atomic.Int64
	size := func() {
		quotes := quoteViews.Get().(*quoteView)
		quotes.source = view
		defer quotes.release()
		for i := int(next.Add(1) - 1); i < len(candidates); i = int(next.Add(1) - 1) {
			o := &outcomes[i]
			o.opportunity, o.issues, o.ok, o.err = e.optimizeCycleObserved(ctx, quotes, candidates[i], nil, &o.limits)
		}
	}
	if workers := min(runtime.GOMAXPROCS(0), len(candidates)); workers > 1 {
		var group sync.WaitGroup
		for range workers {
			group.Go(size)
		}
		group.Wait()
	} else {
		size()
	}
	for i, candidate := range candidates {
		o := outcomes[i]
		opportunity, ok := o.opportunity, o.ok
		result.Selected++
		result.Limits.add(o.limits)
		for _, issue := range o.issues {
			result.Issues = appendIssue(result.Issues, issue)
		}
		if o.err != nil {
			return result, o.err
		}
		e.phantoms.observe(candidate.cycle.Pools, ok && opportunity.Net.Sign() > 0)
		if !ok || opportunity.Net.Sign() <= 0 {
			continue
		}
		e.priceGas(&opportunity)
		result.Opportunities = append(result.Opportunities, opportunity)
	}
	slices.SortFunc(result.Opportunities, func(a, b Opportunity) int {
		if order := bytes.Compare(a.Cycle.Start[:], b.Cycle.Start[:]); order != 0 {
			return order
		}
		return compareOpportunities(a, b)
	})
	return result, nil
}

func (w *rankWorkspace) fundedCycles(snapshot *rankSnapshot, cycle graph.Cycle) ([]fundedRotation, error) {
	w.resize(len(cycle.Pools))
	tokens := w.tokens
	current := cycle.Start
	for i, key := range cycle.Pools {
		tokens[i] = current
		p := snapshot.pools[key]
		if p == nil || !p.ok {
			return nil, nil
		}
		switch current {
		case p.state.Token0:
			current = p.state.Token1
		case p.state.Token1:
			current = p.state.Token0
		default:
			return nil, nil
		}
	}
	if current != cycle.Start {
		return nil, nil
	}
	for i, token := range tokens {
		funding := snapshot.funding[token]
		if !funding.ok {
			continue
		}
		if !funding.valid {
			return nil, fmt.Errorf("%w: funding bounds for %s", ErrInvalidConfig, token)
		}
		w.funded = append(w.funded, fundedRotation{offset: i, token: token, bounds: funding.bounds, low: funding.low, high: funding.high})
	}
	return w.funded, nil
}

func (w *rankWorkspace) estimateRankedCycle(snapshot *rankSnapshot, cycle graph.Cycle, bounds InputBounds) (*big.Int, *big.Int, error) {
	w.legs = slices.Grow(w.legs[:0], len(cycle.Pools))[:len(cycle.Pools)]
	current := cycle.Start
	for i, key := range cycle.Pools {
		leg, next, err := snapshot.leg(key, current)
		if err != nil {
			return nil, nil, err
		}
		w.legs[i] = leg.model
		current = next
	}
	if err := math.EstimateCycleInto(&w.input, &w.net, w.legs, bounds.Min, bounds.Max); err != nil {
		return nil, nil, err
	}
	return &w.input, &w.net, nil
}

func appendIssue(issues []QuoteIssue, issue QuoteIssue) []QuoteIssue {
	for _, existing := range issues {
		if existing.Pool == issue.Pool && existing.Err.Error() == issue.Err.Error() {
			return issues
		}
	}
	return append(issues, issue)
}

func (e *Engine) priceGas(opportunity *Opportunity) {
	opportunity.GasCost = new(big.Int).Set(e.gasCost)
	opportunity.NetAfterGas = new(big.Int).Set(opportunity.Net)
	opportunity.GasComparable = e.gasCost.Sign() == 0 || opportunity.Cycle.Start == e.gasToken
	if opportunity.Cycle.Start == e.gasToken {
		opportunity.NetAfterGas.Sub(opportunity.NetAfterGas, e.gasCost)
	}
}
