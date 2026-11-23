package graph

import (
	"context"
	"fmt"
	"math/bits"
	"slices"
	"sort"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// PackedPath addresses a contiguous range of directed pool IDs.
type PackedPath struct{ Offset, Length uint32 }

// PathLayout is immutable after construction. Consumers may share its arrays,
// but must never modify them. A hop stores poolID*2 + direction.
type PathLayout struct {
	Keys         []domain.PoolKey
	Tokens       []domain.Address
	Pairs        [][2]uint32
	Paths        []PackedPath
	Hops         []uint32
	poolIDs      map[domain.PoolKey]uint32
	offsets      []uint32
	postings     []uint32
	dependencies []uint64
	members      []uint64
}

// PoolID is the pool's index in Keys, which follow pool key order.
func (p *PathLayout) PoolID(key domain.PoolKey) (uint32, bool) {
	id, ok := p.poolIDs[key]
	return id, ok
}

func (p *PathLayout) Path(index int) []uint32 {
	path := p.Paths[index]
	return p.Hops[path.Offset : path.Offset+path.Length]
}

// MarkDependencies unions precomputed co-occurrence masks into caller storage.
// Unindexed dirty pools have no main-table dependencies.
func (p *PathLayout) MarkDependencies(dirty []domain.PoolKey, dst []uint64) []uint64 {
	words := (len(p.Keys) + 63) / 64
	dst = slices.Grow(dst[:0], words)[:words]
	clear(dst)
	for _, key := range dirty {
		id, ok := p.poolIDs[key]
		if !ok {
			continue
		}
		for word, marked := range p.dependencies[int(id)*words : (int(id)+1)*words] {
			dst[word] |= marked
		}
	}
	return dst
}

func (p *PathLayout) DependencyKeys(dirty []domain.PoolKey) []domain.PoolKey {
	mask := p.MarkDependencies(dirty, nil)
	var out []domain.PoolKey
	for word, marked := range mask {
		for marked != 0 {
			id := word*64 + bits.TrailingZeros64(marked)
			marked &= marked - 1
			out = append(out, p.Keys[id])
		}
	}
	out = append(out, dirty...)
	slices.SortFunc(out, domain.ComparePoolKeys)
	return slices.Compact(out)
}

// PackTable returns a table that owns only packed paths and uint32 CSR postings.
// The source stays immutable; callers can release it after adopting this table.
func PackTable(ctx context.Context, source *Table, states []pool.State) (*Table, error) {
	if source == nil {
		return nil, ErrNilCycleSource
	}
	layout, err := CompileLayout(ctx, source, states)
	if err != nil {
		return nil, err
	}
	return &Table{packed: layout}, nil
}

// PackedLayout returns shared read-only storage, or nil for an unpacked table.
func (t *Table) PackedLayout() *PathLayout { return t.packed }

// CompileLayout validates pool identities and token continuity before publishing
// the shared layout. Generic cycle sources retain their own indexing semantics.
func CompileLayout(ctx context.Context, source CycleSource, pools []pool.State) (*PathLayout, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uint64(len(pools)) > uint64(^uint32(0))/2 || uint64(source.Len()) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: packed capacity", ErrInvalidCycle)
	}
	states := slices.Clone(pools)
	slices.SortFunc(states, func(a, b pool.State) int { return domain.ComparePoolKeys(a.Key, b.Key) })
	if packed, ok := source.(interface{ PackedLayout() *PathLayout }); ok && packed.PackedLayout() != nil {
		layout := packed.PackedLayout()
		if len(states) != len(layout.Keys) {
			return nil, fmt.Errorf("%w: packed pool count changed", ErrInvalidPool)
		}
		for i, p := range states {
			pair := layout.Pairs[i]
			if p.Key != layout.Keys[i] || p.Token0 != layout.Tokens[pair[0]] || p.Token1 != layout.Tokens[pair[1]] {
				return nil, fmt.Errorf("%w: packed pool identity changed", ErrInvalidPool)
			}
		}
		return layout, nil
	}
	p := &PathLayout{Paths: make([]PackedPath, source.Len()), poolIDs: make(map[domain.PoolKey]uint32, len(states))}
	if plain, ok := source.(*Table); ok {
		var total uint64
		for _, cycle := range plain.cycles {
			total += uint64(len(cycle.Pools))
		}
		if total > uint64(^uint32(0)) {
			return nil, fmt.Errorf("%w: packed hops exceed capacity", ErrInvalidCycle)
		}
		p.Hops = make([]uint32, 0, int(total))
	}
	if err := p.numberPools(states); err != nil {
		return nil, err
	}
	var storage []domain.PoolKey
	for index := range p.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var cycle Cycle
		if into, ok := source.(interface {
			CycleInto(int, []domain.PoolKey) Cycle
		}); ok {
			cycle = into.CycleInto(index, storage)
			storage = cycle.Pools
		} else {
			cycle = source.Cycle(index)
		}
		if cycle.Len < 2 || cycle.Len != len(cycle.Pools) || uint64(len(p.Hops))+uint64(cycle.Len) > uint64(^uint32(0)) {
			return nil, ErrInvalidCycle
		}
		p.Paths[index] = PackedPath{uint32(len(p.Hops)), uint32(cycle.Len)}
		current := cycle.Start
		for _, key := range cycle.Pools {
			id, ok := p.poolIDs[key]
			if !ok {
				return nil, fmt.Errorf("%w: absent packed pool", ErrInvalidPool)
			}
			state := states[id]
			direction := uint32(0)
			switch current {
			case state.Token0:
				current = state.Token1
			case state.Token1:
				direction, current = 1, state.Token0
			default:
				return nil, fmt.Errorf("%w: disconnected packed path", ErrInvalidCycle)
			}
			p.Hops = append(p.Hops, id*2+direction)
		}
		if current != cycle.Start {
			return nil, fmt.Errorf("%w: packed path does not close", ErrInvalidCycle)
		}
	}
	p.Hops = slices.Clip(p.Hops)
	p.buildIndex()
	p.buildMembership()
	return p, nil
}

// numberPools assigns pool IDs in the sorted order of states and token IDs in
// first-seen order, the identities every packed path refers to.
func (p *PathLayout) numberPools(states []pool.State) error {
	tokens := make(map[domain.Address]uint32)
	for _, state := range states {
		if _, ok := p.poolIDs[state.Key]; ok || state.Token0 == state.Token1 {
			return ErrInvalidPool
		}
		p.poolIDs[state.Key] = uint32(len(p.Keys))
		p.Keys = append(p.Keys, state.Key)
		var pair [2]uint32
		for i, token := range [2]domain.Address{state.Token0, state.Token1} {
			id, ok := tokens[token]
			if !ok {
				id = uint32(len(p.Tokens))
				tokens[token] = id
				p.Tokens = append(p.Tokens, token)
			}
			pair[i] = id
		}
		p.Pairs = append(p.Pairs, pair)
	}
	return nil
}

// buildIndex counts hops through Paths: a truncated dense build can leave hops
// that no path uses, and those must not reach the postings.
func (p *PathLayout) buildIndex() {
	p.offsets = make([]uint32, len(p.Keys)+1)
	for index := range p.Paths {
		for _, hop := range p.Path(index) {
			p.offsets[hop/2+1]++
		}
	}
	for i := 1; i < len(p.offsets); i++ {
		p.offsets[i] += p.offsets[i-1]
	}
	p.postings = make([]uint32, p.offsets[len(p.Keys)])
	cursor := slices.Clone(p.offsets)
	words := (len(p.Keys) + 63) / 64
	p.dependencies = make([]uint64, len(p.Keys)*words)
	for index := range p.Paths {
		path := p.Path(index)
		for _, hop := range path {
			id := hop / 2
			p.postings[cursor[id]] = uint32(index)
			cursor[id]++
			for _, other := range path {
				p.dependencies[int(id)*words+int(other/2)/64] |= uint64(1) << uint(other/2%64)
			}
		}
	}
}

func (p *PathLayout) cycleInto(index int, dst []domain.PoolKey) Cycle {
	if index < 0 || index >= len(p.Paths) {
		return Cycle{}
	}
	path := p.Path(index)
	dst = dst[:0]
	for _, hop := range path {
		dst = append(dst, p.Keys[hop/2])
	}
	first := path[0]
	start := p.Tokens[p.Pairs[first/2][first%2]]
	return Cycle{Pools: dst, Start: start, Len: len(path)}
}

func (p *PathLayout) visit(dirty []domain.PoolKey, k int, visit func(int)) {
	for _, key := range dirty {
		id, ok := p.poolIDs[key]
		if !ok {
			continue
		}
		indices := p.postings[p.offsets[id]:p.offsets[id+1]]
		for _, index := range indices[:min(k, len(indices))] {
			visit(int(index))
		}
	}
}

// Dependencies avoids materializing indexed cycles on packed runtime tables.
// Generic sources preserve their own per-pool index limits through Candidates.
func Dependencies(source CycleSource, dirty []domain.PoolKey) []domain.PoolKey {
	if packed, ok := source.(interface{ PackedLayout() *PathLayout }); ok && packed.PackedLayout() != nil {
		return packed.PackedLayout().DependencyKeys(dirty)
	}
	keys := slices.Clone(dirty)
	for _, index := range source.Candidates(dirty, source.Len()) {
		keys = append(keys, source.Cycle(index).Pools...)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return slices.Compact(keys)
}

// VisitPoolPaths visits the indexed dependencies of one pool ID in path order.
// The callback receives the direction used by that path, not a copied cycle.
func (p *PathLayout) VisitPoolPaths(id uint32, visit func(int, uint32)) {
	p.VisitPoolPathsIn(id, 0, len(p.Paths), visit)
}

// VisitPoolPathsIn is VisitPoolPaths for path indices in [lo, hi). Postings
// are in path order, so callers that own disjoint index ranges can visit the
// same pool concurrently.
func (p *PathLayout) VisitPoolPathsIn(id uint32, lo, hi int, visit func(int, uint32)) {
	postings := p.postings[p.offsets[id]:p.offsets[id+1]]
	start := sort.Search(len(postings), func(i int) bool { return int(postings[i]) >= lo })
	for _, index := range postings[start:] {
		if int(index) >= hi {
			return
		}
		for _, hop := range p.Path(int(index)) {
			if hop/2 == id {
				visit(int(index), hop%2)
				break
			}
		}
	}
}

// PoolPostings is how many paths contain pool id.
func (p *PathLayout) PoolPostings(id uint32) int {
	return int(p.offsets[id+1] - p.offsets[id])
}
