package graph

import (
	"context"
	"math"

	"github.com/Rorical/Rosetta/internal/domain"
)

// unreachable marks a token that cannot lead back to the start token. It is
// small enough that adding a path length cannot overflow.
const unreachable = math.MaxInt32 / 2

// tableBuilder runs the shared depth-first cycle walk over the graph's dense
// indices. Its scratch state is restored by backtracking, so one builder serves
// every start token or anchor edge of a build.
type tableBuilder struct {
	ctx       context.Context // nil in legacy length-band enumeration
	graph     *Graph
	minDepth  int
	maxDepth  int
	maxCycles int
	cycles    []Cycle
	// dense, when set, receives cycles in place of cycles.
	dense     *denseCycles
	seen      map[string]struct{}
	truncated bool

	usedPools  []bool
	visited    []bool
	distance   []int32 // lower bound on pools needed to get back to the start token
	queue      []int32
	pathPools  []int32
	pathTokens []int32 // pathTokens[i] is the token entering pathPools[i]
	key        []byte
}

func newTableBuilder(ctx context.Context, g *Graph, minDepth, maxDepth, maxCycles int) *tableBuilder {
	return &tableBuilder{
		ctx:       ctx,
		graph:     g,
		minDepth:  minDepth,
		maxDepth:  maxDepth,
		maxCycles: maxCycles,
		seen:      make(map[string]struct{}),
		usedPools: make([]bool, len(g.poolKeys)),
		visited:   make([]bool, len(g.tokens)),
		distance:  make([]int32, len(g.tokens)),
		queue:     make([]int32, 0, len(g.tokens)),
	}
}

// enumerate walks from every token in address order. A cycle is recorded only
// from its smallest token, so each walk is confined to tokens no smaller than
// its start; that also makes every closing path start at its smallest token.
func (b *tableBuilder) enumerate() {
	for start := range b.graph.tokens {
		origin := int32(start)
		b.measure(origin, -1, origin)
		b.visited[origin] = true
		stop := b.walk(origin, origin)
		b.visited[origin] = false
		if stop {
			return
		}
	}
}

// walkAnchors walks both directed edges of every anchor pool, in token order,
// and reports whether enumeration stopped early.
func (b *tableBuilder) walkAnchors(anchors []bool) bool {
	for from, links := range b.graph.links {
		for _, next := range links {
			if anchors[next.pool] && b.walkAnchor(int32(from), next) {
				return true
			}
		}
	}
	return false
}

// walkAnchor enumerates cycles that begin with one directed anchor edge.
func (b *tableBuilder) walkAnchor(from int32, anchor link) bool {
	b.measure(from, anchor.pool, 0)
	b.usedPools[anchor.pool] = true
	b.visited[from], b.visited[anchor.to] = true, true
	b.pathPools = append(b.pathPools[:0], anchor.pool)
	b.pathTokens = append(b.pathTokens[:0], from)
	stop := b.walk(anchor.to, from)
	b.pathPools, b.pathTokens = b.pathPools[:0], b.pathTokens[:0]
	b.visited[from], b.visited[anchor.to] = false, false
	b.usedPools[anchor.pool] = false
	return stop
}

// measure fills distance with breadth-first pool counts back to start, never
// crossing skipPool or tokens below floor. Pools are bidirectional, so the
// distance from start equals the distance to it.
func (b *tableBuilder) measure(start, skipPool, floor int32) {
	for i := range b.distance {
		b.distance[i] = unreachable
	}
	b.distance[start] = 0
	queue := append(b.queue[:0], start)
	for head := 0; head < len(queue); head++ {
		token := queue[head]
		for _, next := range b.graph.links[token] {
			if next.pool == skipPool || next.to < floor || b.distance[next.to] != unreachable {
				continue
			}
			b.distance[next.to] = b.distance[token] + 1
			queue = append(queue, next.to)
		}
	}
	b.queue = queue
}

// walk extends a token-simple path of distinct pools from current and records
// every extension that closes on start. Scratch state is restored before
// returning, including when the table fills or the context ends.
func (b *tableBuilder) walk(current, start int32) bool {
	// Checked once per visited token rather than per edge: ctx.Err takes a lock.
	if b.ctx != nil && b.ctx.Err() != nil {
		return true
	}
	depth := len(b.pathPools) + 1 // pools on the path once an edge is taken
	for _, next := range b.graph.links[current] {
		if b.usedPools[next.pool] {
			continue
		}
		if next.to == start {
			// A range build uses minDepth to skip cycles a shorter band already
			// covers, so the merged table does not collect the same short cycle
			// from two ranges.
			if depth < b.minDepth {
				continue
			}
			b.push(next.pool, current)
			stop := b.record()
			b.pop()
			if stop {
				return true
			}
			continue
		}
		// distance ignores used pools and visited tokens, so it never exceeds
		// the true remaining length: this skips only branches that cannot close
		// within maxDepth, and the order of recorded cycles is unchanged.
		if b.visited[next.to] || depth+int(b.distance[next.to]) > b.maxDepth {
			continue
		}
		b.usedPools[next.pool] = true
		b.visited[next.to] = true
		b.push(next.pool, current)
		stop := b.walk(next.to, start)
		b.pop()
		b.visited[next.to] = false
		b.usedPools[next.pool] = false
		if stop {
			return true
		}
	}
	return false
}

func (b *tableBuilder) push(pool, from int32) {
	b.pathPools = append(b.pathPools, pool)
	b.pathTokens = append(b.pathTokens, from)
}

func (b *tableBuilder) pop() {
	b.pathPools = b.pathPools[:len(b.pathPools)-1]
	b.pathTokens = b.pathTokens[:len(b.pathTokens)-1]
}

// record stores the closed path as a new cycle and reports whether enumeration
// must stop because the table is full. An anchored walk can close a cycle from
// any token, so the path is rotated to begin at its smallest token; that gives
// it the same identity as a full enumeration and lets several anchors share one
// deduplication set. A nil seen set skips deduplication.
func (b *tableBuilder) record() bool {
	origin := 0
	for i := 1; i < len(b.pathTokens); i++ {
		if b.pathTokens[i] < b.pathTokens[origin] {
			origin = i
		}
	}
	n := len(b.pathPools)
	start := b.graph.tokens[b.pathTokens[origin]]
	if b.seen != nil {
		b.key = append(b.key[:0], start[:]...)
		for i := range n {
			b.key = appendPoolKeyBytes(b.key, b.graph.poolKeys[b.pathPools[(origin+i)%n]])
		}
		if _, exists := b.seen[string(b.key)]; exists {
			return false
		}
	}
	if b.count() == b.maxCycles {
		b.truncated = true
		return true
	}
	if b.seen != nil {
		b.seen[string(b.key)] = struct{}{}
	}
	if b.dense != nil {
		b.dense.add(b, origin)
		return false
	}
	pools := make([]domain.PoolKey, n)
	for i := range pools {
		pools[i] = b.graph.poolKeys[b.pathPools[(origin+i)%n]]
	}
	b.cycles = append(b.cycles, Cycle{Pools: pools, Start: start, Len: n})
	return false
}

// count is how many cycles the builder holds in either store.
func (b *tableBuilder) count() int {
	if b.dense != nil {
		return len(b.dense.paths)
	}
	return len(b.cycles)
}
