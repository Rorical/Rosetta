package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ExploreLimits bound graph work and retained paths independently of the main
// table. MaxDepth includes the dirty anchor. Each caller owns one Explorer.
type ExploreLimits struct{ MaxDepth, Steps, NewPaths, CachePaths int }

func DefaultExploreLimits(depth int) ExploreLimits {
	return ExploreLimits{MaxDepth: min(16, max(8, depth)), Steps: 4096, NewPaths: 32, CachePaths: 512}
}

// Exploration borrows immutable cached path identities. Consumers must not
// modify their Pools slices; selected search results take independent copies.
type Exploration struct {
	Cycles          []Cycle
	Steps, Added    int
	CatalogPaths    int
	BudgetExhausted bool
	CacheTruncated  bool
}

// Indexed builds postings over the immutable traversal identities. It copies
// cycle headers, while sharing their owned, read-only pool slices. Ordinary
// table getters still return copies to callers.
func (r Exploration) Indexed() *Table {
	table := &Table{cycles: slices.Clone(r.Cycles)}
	table.index()
	return table
}

type exploreJob struct {
	anchor Edge
	depth  int
	cursor *exploreCursor
	done   bool
}
type cachedPath struct {
	cycle   Cycle
	key     string
	used    uint64
	created uint64
}

// Explorer retains traversal cursors and a bounded path cache, never pool state
// or readers. A topology replacement must create a new Explorer; state epochs
// can reuse these graph identities because every quote uses a fresh view.
type Explorer struct {
	graph   *Graph
	indexed *Table
	limits  ExploreLimits
	jobs    []exploreJob
	next    int
	cache   []cachedPath
	order   []int // cache indices in identity order, so results need no sort
	known   map[string]int
	clock   uint64
	evicted bool
}

func NewExplorer(g *Graph, indexed *Table, limits ExploreLimits) (*Explorer, error) {
	if g == nil || indexed == nil {
		return nil, ErrNilGraph
	}
	if limits.MaxDepth < 2 || limits.MaxDepth > 16 || limits.Steps < 1 || limits.NewPaths < 1 || limits.CachePaths < limits.NewPaths {
		return nil, fmt.Errorf("%w: exploration limits", ErrInvalidDepth)
	}
	e := &Explorer{graph: g, indexed: indexed, limits: limits, known: make(map[string]int)}
	var anchors []Edge
	for _, token := range g.tokens {
		for _, edge := range g.adjacency[token] {
			if compareAddresses(edge.From, edge.To) < 0 {
				anchors = append(anchors, edge)
			}
		}
	}
	slices.SortFunc(anchors, func(a, b Edge) int { return domain.ComparePoolKeys(a.Pool, b.Pool) })
	// Rotate over lengths as well as anchors. Exhausting a dense short-path band
	// cannot permanently block the first visit to an eight-hop band.
	for depth := 2; depth <= min(limits.MaxDepth, len(g.tokens)); depth++ {
		for _, anchor := range anchors {
			for direction := 0; direction < 2; direction++ {
				edge := anchor
				if direction == 1 {
					edge.From, edge.To = edge.To, edge.From
				}
				e.jobs = append(e.jobs, exploreJob{anchor: edge, depth: depth})
			}
		}
	}
	return e, nil
}

func (e *Explorer) Explore(ctx context.Context, dirty []domain.PoolKey) (Exploration, error) {
	result := Exploration{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	active := make(map[domain.PoolKey]bool, len(dirty))
	for _, key := range dirty {
		active[key] = true
	}
	e.clock++
	visited := 0
	for visited < len(e.jobs) && result.Steps < e.limits.Steps && result.Added < e.limits.NewPaths {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		job := &e.jobs[e.next]
		e.next = (e.next + 1) % len(e.jobs)
		visited++
		if !active[job.anchor.Pool] {
			continue
		}
		if job.done && e.evicted {
			job.done = false
		}
		if job.done {
			continue
		}
		if job.cursor == nil {
			job.cursor = newExploreCursor(job.anchor, job.depth)
		}
		quantum := min(64, e.limits.Steps-result.Steps)
		steps, done, err := job.cursor.advance(ctx, e.graph, quantum, func(cycle Cycle) bool {
			if e.indexed.ContainsCycle(cycle) {
				return false
			}
			key := cycleKey(cycle)
			if _, exists := e.known[key]; exists {
				return false
			}
			e.insert(cycle, key)
			result.Added++
			return result.Added == e.limits.NewPaths
		})
		result.Steps += steps
		job.done = done
		if done {
			job.cursor = nil
		}
		if err != nil {
			return result, err
		}
	}
	for _, job := range e.jobs {
		if active[job.anchor.Pool] && !job.done {
			result.BudgetExhausted = true
			break
		}
	}
	// Walking in identity order keeps equal-profit tie breaking independent of
	// cache eviction and cursor order without sorting every call.
	for _, i := range e.order {
		entry := &e.cache[i]
		for _, key := range entry.cycle.Pools {
			if active[key] {
				entry.used = e.clock
				result.Cycles = append(result.Cycles, entry.cycle)
				break
			}
		}
	}
	result.CacheTruncated = e.evicted
	return result, nil
}

func compareCycleIdentity(a, b Cycle) int {
	if order := compareAddresses(a.Start, b.Start); order != 0 {
		return order
	}
	return slices.CompareFunc(a.Pools, b.Pools, domain.ComparePoolKeys)
}

func (e *Explorer) insert(cycle Cycle, key string) {
	cycle.Pools = slices.Clone(cycle.Pools)
	e.clock++
	entry := cachedPath{cycle: cycle, key: key, used: e.clock, created: e.clock}
	if len(e.cache) < e.limits.CachePaths {
		e.known[key] = len(e.cache)
		e.cache = append(e.cache, entry)
		e.placeOrdered(len(e.cache) - 1)
		return
	}
	e.evicted = true
	oldest := 0
	for i := 1; i < len(e.cache); i++ {
		if e.cache[i].used < e.cache[oldest].used || (e.cache[i].used == e.cache[oldest].used && e.cache[i].created < e.cache[oldest].created) {
			oldest = i
		}
	}
	delete(e.known, e.cache[oldest].key)
	e.unplaceOrdered(oldest)
	e.cache[oldest] = entry
	e.known[key] = oldest
	e.placeOrdered(oldest)
}

// orderedPosition is where cache entry index belongs in e.order. The known map
// keeps one entry per identity, so positions never tie.
func (e *Explorer) orderedPosition(index int) int {
	position, _ := slices.BinarySearchFunc(e.order, index, func(member, target int) int {
		return compareCycleIdentity(e.cache[member].cycle, e.cache[target].cycle)
	})
	return position
}

func (e *Explorer) placeOrdered(index int) {
	e.order = slices.Insert(e.order, e.orderedPosition(index), index)
}

func (e *Explorer) unplaceOrdered(index int) {
	position := e.orderedPosition(index)
	e.order = slices.Delete(e.order, position, position+1)
}
