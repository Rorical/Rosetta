package arb

import (
	"context"
	"math/big"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// burstMarket is a synthetic market of Arbitrum One's catalog shape; the
// repository holds no recorded Arbitrum state. Twelve hub tokens are pairwise
// joined by four pools each and 180 tail tokens reach three hubs each, 804
// constant-product pools in all, under a depth-4 table capped at 1,000,000
// routes with K=32 and five funding assets. Constant-product quotes are
// cheaper than tick walks, so sizing weighs less here than in production.
type burstMarket struct {
	engine *Engine
	base   map[domain.PoolKey]pool.State
	pools  []pool.State
	// hubPools index pools between two hub tokens: the hot ones.
	hubPools int
	opts     RankedOptions
}

type curveQuoter struct{}

func (curveQuoter) QuoteExactIn(_ context.Context, p pool.State, _ pool.DeepState, token domain.Address, input *big.Int) (*big.Int, error) {
	in, out, _ := traversalReserves(p, token)
	return math.QuoteV2ExactIn(in, out, p.Fee, input)
}

func burstToken(index int) domain.Address {
	return domain.Address{0xb0, byte(index >> 8), byte(index)}
}

func newBurstMarket(b *testing.B) burstMarket {
	b.Helper()
	const hubs, tails = 12, 180
	random := rand.New(rand.NewPCG(7, 11))
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	// A token's price in dollars, times 1000.
	price := make([]int64, hubs+tails)
	for i := range price {
		price[i] = 1 + random.Int64N(4_000_000)
	}
	var pools []pool.State
	add := func(token0, token1 int) {
		// Dollars of depth per side; prices agree to 0.02%, inside every fee.
		depth := big.NewInt(100_000 + random.Int64N(20_000_000))
		reserve := func(token int) *big.Int {
			amount := new(big.Int).Mul(depth, unit)
			amount.Mul(amount, big.NewInt(1000*(10_000+random.Int64N(5)-2)))
			return amount.Quo(amount, big.NewInt(price[token]*10_000))
		}
		key := domain.PoolKey{Address: domain.Address{0xa0, byte(len(pools) >> 8), byte(len(pools))}, Kind: domain.PoolKindV2}
		pools = append(pools, pool.State{Key: key, Token0: burstToken(token0), Token1: burstToken(token1), Reserve0: reserve(token0), Reserve1: reserve(token1), Fee: []uint32{100, 500, 3000}[random.IntN(3)]})
	}
	for a := range hubs {
		for c := a + 1; c < hubs; c++ {
			for range 4 {
				add(a, c)
			}
		}
	}
	hubPools := len(pools)
	for tail := hubs; tail < hubs+tails; tail++ {
		for _, hub := range random.Perm(hubs)[:3] {
			add(hub, tail)
		}
	}
	g, err := graph.Build(pools)
	if err != nil {
		b.Fatal(err)
	}
	table, err := graph.BuildTable(g, 4, 1_000_000)
	if err != nil {
		b.Fatal(err)
	}
	if table, err = graph.PackTable(b.Context(), table, pools); err != nil {
		b.Fatal(err)
	}
	funding := make(map[domain.Address]InputBounds)
	seeds := make(map[domain.Address]*big.Int)
	for hub := range 5 {
		// $0.01 to $1000 with a $100 seed and a $0.005 floor.
		dollars := func(thousandths int64) *big.Int {
			amount := new(big.Int).Mul(unit, big.NewInt(thousandths))
			return amount.Quo(amount, big.NewInt(price[hub]))
		}
		funding[burstToken(hub)] = InputBounds{Min: dollars(10), Max: dollars(1_000_000), Floor: dollars(5)}
		seeds[burstToken(hub)] = dollars(100_000)
	}
	engine, err := New(table, func(token domain.Address) *big.Int { return seeds[token] }, 32, curveQuoter{})
	if err != nil {
		b.Fatal(err)
	}
	if err := engine.PreparePaths(b.Context(), pools); err != nil {
		b.Fatal(err)
	}
	base := make(map[domain.PoolKey]pool.State, len(pools))
	for _, p := range pools {
		base[p.Key] = p
	}
	opts := RankedOptions{StreamInitial: true, Diverse: true, ReservedCPUs: 1,
		SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil },
		Bounds: func(token domain.Address) (InputBounds, bool) {
			bounds, ok := funding[token]
			return bounds, ok
		}}
	return burstMarket{engine: engine, base: base, pools: pools, hubPools: hubPools, opts: opts}
}

type mapView map[domain.PoolKey]pool.State

func (v mapView) Pool(key domain.PoolKey) (pool.State, bool) { state, ok := v[key]; return state, ok }
func (mapView) Deep(domain.PoolKey) (pool.DeepState, bool)   { return pool.DeepState{}, false }

type burstFrame struct {
	dirty            []domain.PoolKey
	at               time.Time
	searched, quoted time.Duration
}

// burstExtra, when set, reports further metrics from the observations.
var burstExtra func(*testing.B, []SearchObservation, float64)

// BenchmarkBurstBlock replays a burst block: 150 frames, alternately 1 ms and
// 2 ms apart, as a Fast Feed burst can deliver in 250 ms.
// A frame is a swap on one to three pools, four in five of them between hubs,
// moving each price by 0.1% to 0.8%. One op is one block on a fresh worker;
// the consumer accepts what passes the per-pool dependency proof and models
// no queued-input gate. Run with -cpu set to the search core count.
// searched-ms is the time from a frame to the end of the first
// completed search that included it; short-ms the time to the first accepted
// quote of at most three pools through one of its pools, from such a search.
// Both are quantiles over all 150 frames: a frame the block ends without
// counts as the block's length. short-quotes and long-quotes count accepted
// sized quotes; rank-cpu-wait and queue cover every search that started.
//
// "quiet" sends 12 such frames 80 ms apart, each search finishing before the
// next frame, to compare a search that nothing interrupts.
func BenchmarkBurstBlock(b *testing.B) {
	market := newBurstMarket(b)
	b.Run("burst", func(b *testing.B) {
		burstBlock(b, market, 150, 250*time.Millisecond, func(index int) time.Duration { return time.Duration(1+index%2) * time.Millisecond })
	})
	b.Run("quiet", func(b *testing.B) {
		burstBlock(b, market, 12, time.Second, func(int) time.Duration { return 80 * time.Millisecond })
	})
}

func burstBlock(b *testing.B, market burstMarket, frames int, blockTime time.Duration, gap func(int) time.Duration) {
	var observations []SearchObservation
	var searched, quoted, rankWait, queue, evaluation []time.Duration
	covered, sizedFrames, shortQuotes, longQuotes := 0, 0, 0, 0
	b.ResetTimer()
	for block := 0; b.Loop(); block++ {
		ctx, cancel := context.WithCancel(b.Context())
		w, err := NewWorker(ctx, market.engine, market.opts)
		if err != nil {
			b.Fatal(err)
		}
		var mu sync.Mutex
		burst := make([]burstFrame, 0, frames)
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			for event := range w.Results() {
				origin := int(event.Job.Version.TxIndex)
				accepted := w.Rebase(&event) && w.Accept(event) && event.Partial
				if accepted && event.Stage == QuoteOptimized {
					mu.Lock()
					if len(event.Result.Opportunities[0].Cycle.Pools) <= shortRouteHops {
						shortQuotes++
					} else {
						longQuotes++
					}
					mu.Unlock()
				}
				if accepted && len(event.Result.Opportunities[0].Cycle.Pools) <= shortRouteHops {
					now, route := time.Now(), event.Result.Opportunities[0].Cycle.Pools
					mu.Lock()
					for index := range min(origin+1, len(burst)) {
						frame := &burst[index]
						if frame.quoted == 0 && slices.ContainsFunc(frame.dirty, func(key domain.PoolKey) bool { return slices.Contains(route, key) }) {
							frame.quoted = now.Sub(frame.at)
						}
					}
					mu.Unlock()
				}
				releaseSearch(event.Job)
			}
		}()
		random := rand.New(rand.NewPCG(uint64(block), 3))
		view := mapView(market.base)
		for index := range frames {
			next := make(mapView, len(view))
			for key, state := range view {
				next[key] = state
			}
			var dirty []domain.PoolKey
			for range 1 + random.IntN(10)/6 + random.IntN(10)/9 {
				at := random.IntN(market.hubPools)
				if random.IntN(5) == 0 {
					at = market.hubPools + random.IntN(len(market.pools)-market.hubPools)
				}
				state := next[market.pools[at].Key]
				// A swap of token0 in: reserve0 grows and reserve1 shrinks by the same factor.
				move := int64(1000 + 1 + random.IntN(8))
				if random.IntN(2) == 0 {
					state.Reserve0, state.Reserve1 = state.Reserve1, state.Reserve0
					state.Token0, state.Token1 = state.Token1, state.Token0
				}
				grown := new(big.Int).Mul(state.Reserve0, big.NewInt(move))
				shrunk := new(big.Int).Mul(state.Reserve1, big.NewInt(1000))
				state.Reserve0, state.Reserve1 = grown.Quo(grown, big.NewInt(1000)), shrunk.Quo(shrunk, big.NewInt(move))
				if state.Token0 != market.pools[at].Token0 {
					state.Reserve0, state.Reserve1 = state.Reserve1, state.Reserve0
					state.Token0, state.Token1 = state.Token1, state.Token0
				}
				next[state.Key] = state
				dirty = append(dirty, state.Key)
			}
			slices.SortFunc(dirty, domain.ComparePoolKeys)
			dirty = slices.Compact(dirty)
			view = next
			now := time.Now()
			mu.Lock()
			burst = append(burst, burstFrame{dirty: dirty, at: now})
			mu.Unlock()
			job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, ReceivedAt: now,
				Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: uint64(block + 1), TxIndex: uint64(index)}}
			job.Observe = func(o SearchObservation) {
				done := time.Now()
				mu.Lock()
				defer mu.Unlock()
				observations = append(observations, o)
				if o.Outcome != SearchSuperseded {
					rankWait, queue = append(rankWait, o.Stages.RankCPU), append(queue, o.QueueWait)
				}
				if o.Outcome != SearchCompleted {
					return
				}
				evaluation = append(evaluation, o.Evaluation)
				for earlier := range burst[:index+1] {
					if burst[earlier].searched == 0 {
						burst[earlier].searched = done.Sub(burst[earlier].at)
					}
				}
			}
			if err := w.Submit(job); err != nil {
				b.Fatal(err)
			}
			time.Sleep(gap(index))
		}
		// The block ends 250 ms after it began; what is unfinished then is lost
		// to the next block's boundary.
		time.Sleep(blockTime - time.Since(burst[0].at))
		cancel()
		<-w.Done()
		<-consumed
		for _, frame := range burst {
			if frame.searched > 0 {
				covered++
			} else {
				frame.searched = blockTime
			}
			if frame.quoted > 0 {
				sizedFrames++
			} else {
				frame.quoted = blockTime
			}
			searched, quoted = append(searched, frame.searched), append(quoted, frame.quoted)
		}
	}
	b.StopTimer()
	blocks := float64(b.N)
	outcomes := map[SearchOutcome]int{}
	for _, o := range observations {
		outcomes[o.Outcome]++
	}
	quantile := func(values []time.Duration, q float64) float64 {
		if len(values) == 0 {
			return 0
		}
		slices.Sort(values)
		return float64(values[int(q*float64(len(values)-1))].Microseconds()) / 1000
	}
	b.ReportMetric(float64(outcomes[SearchCompleted])/blocks, "completed/block")
	b.ReportMetric(float64(outcomes[SearchCanceled])/blocks, "canceled/block")
	b.ReportMetric(float64(outcomes[SearchSuperseded])/blocks, "superseded/block")
	b.ReportMetric(float64(covered)/blocks, "frames-searched/block")
	b.ReportMetric(quantile(searched, 0.5), "searched-p50-ms")
	b.ReportMetric(quantile(searched, 0.9), "searched-p90-ms")
	b.ReportMetric(float64(sizedFrames)/blocks, "frames-short-quoted/block")
	b.ReportMetric(quantile(quoted, 0.5), "short-p50-ms")
	b.ReportMetric(quantile(quoted, 0.9), "short-p90-ms")
	b.ReportMetric(float64(shortQuotes)/blocks, "short-quotes/block")
	b.ReportMetric(float64(longQuotes)/blocks, "long-quotes/block")
	b.ReportMetric(quantile(rankWait, 0.5), "rank-cpu-wait-p50-ms")
	b.ReportMetric(quantile(rankWait, 0.9), "rank-cpu-wait-p90-ms")
	b.ReportMetric(quantile(evaluation, 0.5), "evaluation-p50-ms")
	b.ReportMetric(quantile(queue, 0.5), "queue-p50-ms")
	b.ReportMetric(quantile(queue, 0.9), "queue-p90-ms")
	if burstExtra != nil {
		burstExtra(b, observations, blocks)
	}
}
