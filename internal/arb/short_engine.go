package arb

import (
	"context"
	"errors"
	"fmt"
	poolmath "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
	"maps"
	stdmath "math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// shortCycles holds the few short routes through each pool. A fast lane looks
// them up per frame without the main table's catalog exploration. It keeps no
// route of its own: route i is source route held[i]. Clones of every route and
// string route keys would hold about 94% of a 1M-route topology's heap when the
// fast lane is as deep as the full search.
type shortCycles struct {
	source CycleTable
	held   []int32
	byPool map[domain.PoolKey][]int
	// cap bounds how many routes per dirty pool the fast lane ranks, after
	// byPool is sorted shortest-first. Zero ranks them all. The full search
	// worker still covers every route, so the cap only defers a route beyond
	// it to a later frame, never drops it.
	cap int
	// routes finds a route by its pools: hash<<32 | route, sorted, with equal
	// hashes resolved against the held route. Hashes read pool IDs in key
	// order: a packed source's own (layout), or ones numbered over a plain
	// source's pools (ids). hot holds, per pool, routes that recently quoted a
	// profit (MarkHot): they rank past the cap until hotRouteTTL passes. Both
	// are used only on the fast lane's goroutine.
	routes []uint64
	layout *graph.PathLayout
	ids    map[domain.PoolKey]uint32
	hot    map[domain.PoolKey]map[int]time.Time
	// found is when the full search last found each route profitable, kept
	// past hotRouteTTL to tell a returning route from a new one (FastCoverage).
	found     map[int]time.Time
	hotPurged time.Time
}

// hotRouteTTL is how long a route that quoted a profit stays a fast-lane
// candidate past the per-pool cap. Most of the full search's admitted routes
// can have four hops, which the cap pushes out. Admitted routes recur: about
// half were admitted within the previous 10 minutes and a further sixth within
// the hour.
const hotRouteTTL = time.Hour

// hotRoutesPerPool bounds the hot routes ranked through one pool; the oldest
// make way. Unbounded, a hub pool gathers enough that fast-lane quotes take
// about 55 ms on the main loop instead of 2 ms.
const hotRoutesPerPool = 64

// postedHops is the longest route ranked through a dirty pool by default;
// longer routes the engine holds rank only while hot. Posting four-hop routes
// too filled the per-pool cap with them on pools with few shorter routes.
const postedHops = 3

// routeKey identifies a route whatever pool it is reported from: the full
// search rotates a route to start at its funding asset, while the table holds
// it from its enumeration start. The pools are read from the smallest key, so
// a route and its rotations share a key. The two directions of a two-pool
// route also share one; both are then marked, which costs one extra quote.
func routeKey(cycle graph.Cycle) string {
	return string(appendRouteKey(nil, cycle.Pools))
}

func appendRouteKey(dst []byte, pools []domain.PoolKey) []byte {
	first := 0
	for i, p := range pools {
		if domain.ComparePoolKeys(p, pools[first]) < 0 {
			first = i
		}
	}
	for i := range pools {
		p := pools[(first+i)%len(pools)]
		dst = append(dst, p.Address[:]...)
		dst = append(dst, byte(p.Kind))
		dst = append(dst, p.PoolID[:]...)
		dst = append(dst, p.Hooks[:]...)
	}
	return dst
}

// routeHash is FNV-1a over a route's pool IDs read from the smallest, the
// order routeKey reads its pools in.
func routeHash(ids []uint32) uint64 {
	first := smallest(ids)
	hash := uint32(2166136261)
	for i := range ids {
		hash = (hash ^ ids[(first+i)%len(ids)]) * 16777619
	}
	return uint64(hash) << 32
}

func smallest(ids []uint32) int {
	first := 0
	for i, id := range ids {
		if id < ids[first] {
			first = i
		}
	}
	return first
}

// sameRoute compares two pool ID sequences read from their smallest ID.
func sameRoute(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	i, j := smallest(a), smallest(b)
	for k := range a {
		if a[(i+k)%len(a)] != b[(j+k)%len(b)] {
			return false
		}
	}
	return true
}

// poolIDs maps a route's pools to the index's IDs; ok is false for a pool
// the fast lane does not hold.
func (c *shortCycles) poolIDs(pools []domain.PoolKey, dst []uint32) ([]uint32, bool) {
	dst = dst[:0]
	for _, key := range pools {
		var id uint32
		var ok bool
		if c.layout != nil {
			id, ok = c.layout.PoolID(key)
		} else {
			id, ok = c.ids[key]
		}
		if !ok {
			return nil, false
		}
		dst = append(dst, id)
	}
	return dst, true
}

// heldIDs reads the pool IDs of a source route.
func (c *shortCycles) heldIDs(index int, dst []uint32, pools []domain.PoolKey) ([]uint32, []domain.PoolKey) {
	if c.layout != nil {
		dst = dst[:0]
		for _, hop := range c.layout.Path(index) {
			dst = append(dst, hop/2)
		}
		return dst, pools
	}
	pools = readCycle(c.source, index, pools).Pools
	dst, _ = c.poolIDs(pools, dst)
	return dst, pools
}

// lookup returns, in route order, the held routes that share cycle's key.
func (c *shortCycles) lookup(cycle graph.Cycle) []int {
	ids, ok := c.poolIDs(cycle.Pools, nil)
	if !ok || len(ids) == 0 {
		return nil
	}
	hash := routeHash(ids)
	var out []int
	var held []uint32
	var pools []domain.PoolKey
	for i := sort.Search(len(c.routes), func(i int) bool { return c.routes[i] >= hash }); i < len(c.routes) && c.routes[i]&^stdmath.MaxUint32 == hash; i++ {
		route := int(uint32(c.routes[i]))
		if held, pools = c.heldIDs(int(c.held[route]), held, pools); sameRoute(held, ids) {
			out = append(out, route)
		}
	}
	return out
}

func (c *shortCycles) markHot(cycle graph.Cycle, now time.Time) {
	indices := c.lookup(cycle)
	if len(indices) == 0 {
		return
	}
	if c.hot == nil {
		c.hot = make(map[domain.PoolKey]map[int]time.Time)
	}
	if now.Sub(c.hotPurged) >= hotRouteTTL {
		for key, routes := range c.hot {
			maps.DeleteFunc(routes, func(_ int, at time.Time) bool { return now.Sub(at) >= hotRouteTTL })
			if len(routes) == 0 {
				delete(c.hot, key)
			}
		}
		c.hotPurged = now
	}
	for _, index := range indices {
		c.markIndexHot(index, now)
	}
}

func (c *shortCycles) markIndexHot(index int, now time.Time) {
	if c.found == nil {
		c.found = make(map[int]time.Time)
	}
	c.found[index] = now
	for _, key := range c.Cycle(index).Pools {
		routes := c.hot[key]
		if routes == nil {
			routes = make(map[int]time.Time)
			c.hot[key] = routes
		}
		if _, ok := routes[index]; !ok && len(routes) >= hotRoutesPerPool {
			oldest, at := -1, now
			for i, t := range routes {
				if oldest < 0 || t.Before(at) {
					oldest, at = i, t
				}
			}
			delete(routes, oldest)
		}
		routes[index] = now
	}
}

func (c *shortCycles) Len() int                    { return len(c.held) }
func (c *shortCycles) Cycle(index int) graph.Cycle { return c.CycleInto(index, nil) }

// CycleInto reads the route from the source table into dst's storage, so
// ranking reuses one buffer per workspace instead of allocating per read.
func (c *shortCycles) CycleInto(index int, dst []domain.PoolKey) graph.Cycle {
	if index < 0 || index >= len(c.held) {
		return graph.Cycle{}
	}
	return readCycle(c.source, int(c.held[index]), dst)
}

func readCycle(table CycleTable, index int, dst []domain.PoolKey) graph.Cycle {
	if into, ok := table.(interface {
		CycleInto(int, []domain.PoolKey) graph.Cycle
	}); ok {
		return into.CycleInto(index, dst)
	}
	return table.Cycle(index)
}

func (c *shortCycles) Candidates(dirty []domain.PoolKey, k int) []int {
	if k < 1 {
		return nil
	}
	var out []int
	now := time.Now()
	for _, key := range dirty {
		postings := c.byPool[key]
		if c.cap > 0 && len(postings) > c.cap {
			postings = postings[:c.cap]
		}
		out = append(out, postings...)
		for index, at := range c.hot[key] {
			if now.Sub(at) < hotRouteTTL {
				out = append(out, index)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ShortEngine returns an engine over this engine's routes of at most maxLen
// hops, sharing its quoter, notional and gas pricing, with k exact quotes per
// funding asset. A compiled engine's fast lane ranks through its own paths
// over the same packed hops. Building it walks the whole table once; do it
// off the hot path.
func (e *Engine) ShortEngine(ctx context.Context, maxLen, k, cap int) (*Engine, error) {
	short, err := newShortCycles(ctx, e.table, maxLen, cap)
	if err != nil {
		return nil, err
	}
	fast, err := New(short, e.notional, k, e.quoter)
	if err != nil {
		return nil, err
	}
	fast.gasCost, fast.gasToken = new(big.Int).Set(e.gasCost), e.gasToken
	if e.compiled != nil {
		paths := make([]graph.PackedPath, len(short.held))
		for i, index := range short.held {
			paths[i] = e.compiled.paths[index]
		}
		fast.compiled = &compiledPaths{keys: e.compiled.keys, tokens: e.compiled.tokens, pairs: e.compiled.pairs, paths: paths, hops: e.compiled.hops}
	}
	return fast, nil
}

func newShortCycles(ctx context.Context, table CycleTable, maxLen, cap int) (*shortCycles, error) {
	if table.Len() > stdmath.MaxInt32 {
		return nil, fmt.Errorf("%w: %d routes exceed the fast lane's index", ErrInvalidConfig, table.Len())
	}
	short := &shortCycles{source: table, byPool: make(map[domain.PoolKey][]int), cap: cap}
	if packed, ok := table.(interface{ PackedLayout() *graph.PathLayout }); ok {
		short.layout = packed.PackedLayout()
	}
	var keys []domain.PoolKey
	if short.layout != nil {
		keys = short.layout.Keys
	} else {
		short.ids, keys = numberPools(table)
	}
	var lengths []uint8
	var ids []uint32
	var pools []domain.PoolKey
	for i := 0; i < table.Len(); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ids, pools = short.heldIDs(i, ids, pools)
		if len(ids) > maxLen {
			continue
		}
		route := len(short.held)
		short.held = append(short.held, int32(i))
		lengths = append(lengths, uint8(min(len(ids), stdmath.MaxUint8)))
		short.routes = append(short.routes, routeHash(ids)|uint64(route))
		if len(ids) > postedHops {
			continue // reachable only as a hot route
		}
		for _, id := range ids {
			key := keys[id]
			if postings := short.byPool[key]; len(postings) == 0 || postings[len(postings)-1] != route {
				short.byPool[key] = append(postings, route)
			}
		}
	}
	slices.Sort(short.routes)
	// Rank shorter routes first: they quote cheaper and the cap keeps them
	// over longer ones, which the full search still covers.
	for key, postings := range short.byPool {
		slices.SortFunc(postings, func(a, b int) int {
			if d := int(lengths[a]) - int(lengths[b]); d != 0 {
				return d
			}
			return a - b
		})
		short.byPool[key] = postings
	}
	return short, nil
}

// numberPools gives a plain table's pools IDs in key order, as a packed
// layout numbers them.
func numberPools(table CycleTable) (map[domain.PoolKey]uint32, []domain.PoolKey) {
	seen := make(map[domain.PoolKey]bool)
	var keys []domain.PoolKey
	var pools []domain.PoolKey
	for i := range table.Len() {
		pools = readCycle(table, i, pools).Pools
		for _, key := range pools {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	ids := make(map[domain.PoolKey]uint32, len(keys))
	for id, key := range keys {
		ids[key] = uint32(id)
	}
	return ids, keys
}

// MarkHot keeps a route that just quoted a profit among this fast-lane
// engine's candidates past its per-pool cap for hotRouteTTL. Routes the engine
// does not hold, and engines that are not fast lanes, ignore it. Call it on
// the goroutine that ranks with this engine.
func (e *Engine) MarkHot(cycle graph.Cycle, now time.Time) {
	if short, ok := e.table.(*shortCycles); ok {
		short.markHot(cycle, now)
	}
}

// RouteKey identifies a route independently of the pool it is reported from.
func RouteKey(cycle graph.Cycle) string { return routeKey(cycle) }

// Fast-lane coverage of a route for one frame (FastCoverage).
const (
	CoverageNotHeld      = "not_held"      // longer than the fast lane's hops, or not in its table
	CoverageNotCandidate = "not_candidate" // held, but past the per-pool cap of every dirty pool and not hot
	CoverageCandidate    = "candidate"     // ranked for this frame
)

// FastCoverage reports whether this fast-lane engine ranks cycle for a frame
// that changed dirty. Call it on the goroutine that ranks with this engine.
func (e *Engine) FastCoverage(cycle graph.Cycle, dirty []domain.PoolKey, now time.Time) string {
	short, ok := e.table.(*shortCycles)
	if !ok {
		return CoverageNotHeld
	}
	indices := short.lookup(cycle)
	if len(indices) == 0 {
		return CoverageNotHeld
	}
	for _, key := range dirty {
		postings := short.byPool[key]
		if short.cap > 0 && len(postings) > short.cap {
			postings = postings[:short.cap]
		}
		for _, index := range indices {
			if slices.Contains(postings, index) {
				return CoverageCandidate
			}
			if at, hot := short.hot[key][index]; hot && now.Sub(at) < hotRouteTTL {
				return CoverageCandidate
			}
		}
	}
	return CoverageNotCandidate + "_" + short.lastFound(indices, now)
}

// lastFound buckets how long ago the full search last found any of indices
// profitable: within hotRouteTTL (so pushed out of hot by hotRoutesPerPool),
// within six hours, longer, or never since start.
func (c *shortCycles) lastFound(indices []int, now time.Time) string {
	var latest time.Time
	for _, index := range indices {
		if at := c.found[index]; at.After(latest) {
			latest = at
		}
	}
	age := now.Sub(latest)
	switch {
	case latest.IsZero():
		return "never_found"
	case age < hotRouteTTL:
		return "found_evicted"
	case age < 6*time.Hour:
		return "found_6h"
	default:
		return "found_older"
	}
}

// Why the fast lane did not quote a route it ranks for a frame (DiagnoseRoute).
const (
	DiagnosisOptimizeDropped = "optimize_dropped" // selected for the frame; exact sizing found no publishable quote
	DiagnosisRankCut         = "rank_cut"         // ranks on its own, but below this engine's K for the frame
	DiagnosisCoarsePruned    = "coarse_pruned"    // the coarse model estimates no profit for it
)

// DiagnoseRoute ranks the frame again and then cycle on its own to say where
// this engine's evaluation dropped cycle. It costs two ranks; call it rarely,
// on the goroutine that ranks with this engine.
func (e *Engine) DiagnoseRoute(ctx context.Context, view View, cycle graph.Cycle, dirty []domain.PoolKey, opts RankedOptions) (string, error) {
	key := routeKey(cycle)
	selection, err := e.Rank(ctx, view, dirty, opts)
	if err != nil {
		return "", err
	}
	if candidate, ok := selection.route(key); ok {
		return e.diagnoseSizing(ctx, view, key, candidate, selection, opts)
	}
	alone, err := New(exploredCycles{cycle}, e.notional, int(e.k.Load()), e.quoter)
	if err != nil {
		return "", err
	}
	alone.gasCost, alone.gasToken = e.gasCost, e.gasToken
	selection, err = alone.Rank(ctx, view, cycle.Pools, opts)
	if err != nil {
		return "", err
	}
	if !selection.hasRoute(key) {
		return DiagnosisCoarsePruned, nil
	}
	// Rank the frame with a wide K to find how deep the coarse order put it.
	wide, err := New(e.table, e.notional, diagnosisRankDepth, e.quoter)
	if err != nil {
		return "", err
	}
	wide.gasCost, wide.gasToken = e.gasCost, e.gasToken
	selection, err = wide.Rank(ctx, view, dirty, opts)
	if err != nil {
		return "", err
	}
	position := selection.position(key)
	switch {
	case position < 0:
		return DiagnosisRankCut + "_beyond_" + strconv.Itoa(diagnosisRankDepth), nil
	case position < 8:
		return DiagnosisRankCut + "_top8", nil
	case position < 32:
		return DiagnosisRankCut + "_top32", nil
	default:
		return DiagnosisRankCut + "_top" + strconv.Itoa(diagnosisRankDepth), nil
	}
}

// diagnosisRankDepth is how deep DiagnoseRoute looks into a frame's coarse order.
const diagnosisRankDepth = 256

// diagnoseSizing sizes one selected candidate on a scratch engine, so the
// diagnosis leaves this engine's phantom-pool record alone, and says why the
// frame's evaluation published nothing for it.
func (e *Engine) diagnoseSizing(ctx context.Context, view View, key string, candidate rankedCycle, frame RankedSelection, opts RankedOptions) (string, error) {
	probe, err := New(e.table, e.notional, int(e.k.Load()), e.quoter)
	if err != nil {
		return "", err
	}
	probe.gasCost, probe.gasToken = e.gasCost, e.gasToken
	result, err := probe.Optimize(ctx, view, RankedSelection{candidates: []rankedCycle{candidate}})
	if err != nil {
		return DiagnosisOptimizeDropped + "_error", nil
	}
	for _, o := range result.Opportunities {
		if !opts.publishable(o) {
			return DiagnosisOptimizeDropped + "_below_floor", nil
		}
		// Size the whole frame selection the same way: if the route is
		// published there too, the frame's own evaluation saw other input.
		whole, err := probe.Optimize(ctx, view, frame)
		if err != nil {
			return DiagnosisOptimizeDropped + "_error", nil
		}
		for _, w := range whole.Opportunities {
			if routeKey(w.Cycle) == key && opts.publishable(w) {
				return DiagnosisOptimizeDropped + "_publishable_in_frame", nil
			}
		}
		return DiagnosisOptimizeDropped + "_publishable_alone_only", nil
	}
	if len(result.Issues) > 0 {
		return DiagnosisOptimizeDropped + "_" + issueClass(result.Issues[0].Err), nil
	}
	return DiagnosisOptimizeDropped + "_unprofitable", nil
}

// issueClass folds a quote issue into a bounded label for counters.
func issueClass(err error) string {
	switch {
	case errors.Is(err, ErrMissingPool):
		return "missing_pool"
	case errors.Is(err, ErrMissingDeep), errors.Is(err, poolmath.ErrMissingTick):
		return "missing_ticks"
	case errors.Is(err, poolmath.ErrOutOfWindow):
		return "out_of_window"
	case errors.Is(err, poolmath.ErrCapacity):
		return "capacity"
	case errors.Is(err, pool.ErrNoLiquidity):
		return "no_liquidity"
	default:
		return "other_issue"
	}
}
