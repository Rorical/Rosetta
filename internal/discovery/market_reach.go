package discovery

import (
	"math"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// farAway marks a token beyond the search limit; two of them still sum
// without overflow.
const farAway = math.MaxInt32 / 4

// additionReach bounds where added pools can create cycles. A new cycle
// through anchor A and added pool P splits into two token-disjoint paths that
// join A's tokens to P's tokens without using A or P, with at most limit pools
// between them. Breadth-first distances from P's tokens in the graph without P
// can only underestimate those paths, so an anchor that fails the bound for
// every added pool keeps exactly its old cycle set.
type additionReach struct {
	tokens map[domain.Address]int32
	pools  []domain.PoolKey
	// from[i] holds hop distances from added pool i's token0 and token1.
	from  [][2][]int32
	limit int32
}

func newAdditionReach(eligible, added []pool.State, depth int) *additionReach {
	// The short pass always looks for two-pool cycles, even below depth 2.
	r := &additionReach{tokens: map[domain.Address]int32{}, limit: int32(max(depth, 2) - 2)}
	for _, p := range eligible {
		for _, token := range []domain.Address{p.Token0, p.Token1} {
			if _, ok := r.tokens[token]; !ok {
				r.tokens[token] = int32(len(r.tokens))
			}
		}
	}
	type hop struct{ pool, to int32 }
	links := make([][]hop, len(r.tokens))
	positions := make(map[domain.PoolKey]int32, len(eligible))
	for i, p := range eligible {
		a, b := r.tokens[p.Token0], r.tokens[p.Token1]
		links[a] = append(links[a], hop{pool: int32(i), to: b})
		links[b] = append(links[b], hop{pool: int32(i), to: a})
		positions[p.Key] = int32(i)
	}
	queue := make([]int32, 0, len(r.tokens))
	measure := func(start, skip int32) []int32 {
		distance := make([]int32, len(r.tokens))
		for i := range distance {
			distance[i] = farAway
		}
		distance[start] = 0
		queue = append(queue[:0], start)
		for head := 0; head < len(queue); head++ {
			token := queue[head]
			if distance[token] >= r.limit {
				continue
			}
			for _, next := range links[token] {
				if next.pool == skip || distance[next.to] != farAway {
					continue
				}
				distance[next.to] = distance[token] + 1
				queue = append(queue, next.to)
			}
		}
		return distance
	}
	for _, p := range added {
		skip := positions[p.Key]
		r.pools = append(r.pools, p.Key)
		r.from = append(r.from, [2][]int32{measure(r.tokens[p.Token0], skip), measure(r.tokens[p.Token1], skip)})
	}
	return r
}

// touches reports whether some added pool other than the anchor could share
// a cycle within the depth limit with it.
func (r *additionReach) touches(anchor domain.PoolKey, tokens tokenPair) bool {
	a, okA := r.tokens[tokens[0]]
	b, okB := r.tokens[tokens[1]]
	// A token outside the graph has no pool but the anchor, so it cannot be on
	// a cycle through any added pool.
	if !okA || !okB {
		return false
	}
	for i, key := range r.pools {
		if key == anchor {
			continue
		}
		from0, from1 := r.from[i][0], r.from[i][1]
		if from0[a]+from1[b] <= r.limit || from1[a]+from0[b] <= r.limit {
			return true
		}
	}
	return false
}
