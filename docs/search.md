# Search

Search finds closed cycles of swaps (token A → … → token A) whose output exceeds their input at the current pool state, and sizes the input that maximizes profit. It runs every time pools change, so most of the design is about doing little work per change: a prebuilt route catalog, an index from pools to routes, a cheap ranking model, and exact integer quotes only for the few routes that survive ranking.

Search is bounded by route count, depth, K and CPU. It reports the best routes it found within those bounds and makes no claim of global optimality.

Code: `internal/graph` (catalog, postings, exploration), `internal/arb` (ranking, sizing, the search worker), `internal/math` (pure math), `internal/pool` and `internal/dex/<protocol>` (adapters).

## Pipeline

```text
receipt logs ─> pool store ─> dirty pools
   ─> routes through dirty pools (catalog postings, observed routes, explored routes)
   ─> coarse ranking with a shallow model, in batches, short routes first
   ─> K candidates per funding asset (value plus diversity)
   ─> exact amount sizing with protocol integer math
   ─> each positive quote is published as soon as it exists
```

Ranking and sizing overlap. The first batches of two- and three-hop routes are ranked and sized before longer routes are ranked, and candidates leave as they finish instead of waiting for the whole frame.

## Graph and route catalog

Nodes are tokens. Each pool contributes two directed edges, one per swap direction. Edges hold pool identity only; prices are read from pool state when a route is ranked, so a price update changes no graph structure.

The graph holds only pools that have a quoting adapter, have been verified against their factory or catalog, and are not quarantined. Uniswap V4 pools with non-zero hooks are excluded, because a hook can change the fee or the swap itself. Routes may not repeat a pool or an intermediate token.

### Building the catalog

The catalog (`graph.Table`) is a precomputed list of simple cycles of 2 to `max_depth` hops (at most 16). Two properties drive its design:

- The number of cycles grows roughly by the graph's branching factor per hop, so a complete table at depth four or more can hold millions of routes.
- Most profitable cycles are short, but a new market can be reachable only through long routes.

`graph.BuildBalancedTableContext` (`internal/graph/balanced.go`) therefore fills the table by length, shortest first. Every length that fits is enumerated completely. For the first length that does not fit, and longer ones, each pool and direction is seeded with routes first, so no pool is left without coverage; the remaining budget then fills shortest first. A full table reports truncation (`ErrTableFull`).

`max_cycles` sets the budget. `"auto"` sizes it from the smaller of a memory limit (a quarter of `GOMEMLIMIT`, the cgroup limit or physical memory) and a per-core limit (`pipeline.AutoMaxCycles`).

### Compact layout

The runtime table stores each hop as `poolID*2 + direction` in one shared array; a route is an offset and a length (`graph.PathLayout` in `internal/graph/packed.go`). Postings from pool to routes use a CSR index of `uint32`. The builder writes this layout directly, without materializing a per-route object, and enumerates lengths that fit in parallel by start token and first pool, then concatenates in serial order so the result is identical to a single-threaded build.

### Incremental growth

When discovery adds pools, `graph.ExtendPackedContext` (`internal/graph/extend.go`) keeps every existing route and its index and enumerates only cycles through the new pools. When the table is full, a background rebuild replaces it. A new graph version retires old route IDs and every cache keyed on them; the new table and its index are adopted together on the state writer.

### Routes outside the catalog

Two bounded sources add routes the catalog does not hold:

| Source | Behavior |
|---|---|
| Background exploration (`internal/arb/background_paths.go`, `internal/graph/explore.go`) | A worker walks the whole graph, rotating anchor pools, directions and lengths. Depth is between 8 and 16 hops. Each pass takes a bounded number of steps and new paths, using spare CPU only. Found paths live in a bounded cache (8192 by default) with its own pool postings |
| Observed routes (`internal/arb/observed_paths.go`) | The canonical writer extracts supported closed cycles from committed transactions and remembers up to 4096 of them. They are kept apart from the explorer cache so traversal cannot evict them, and are revalidated when the graph changes |

Both sources store route identity only. Every search quotes them at current state, and they compete for the same K as catalog routes.

## Dirty routes and search jobs

The state writer publishes a version and the set of pools that changed. The search worker (`arb.Worker`) keeps a record of "owed" pools per source: pools whose routes still need a search at the current block lineage (`internal/arb/owed_pools.go`).

Each job runs in two phases (`internal/arb/search_run.go`):

- The short phase ranks every route of at most three hops through pools owed their short routes, selects K per asset and sizes them.
- The long phase ranks longer routes and the supplemental catalog for pools owed their long routes, after the job's short quotes are out.

A source runs at most four jobs at once (`laneRunLimit`): one ranking short routes, one ranking long routes, and two still sizing. A job that finds the long slot taken ends after its short phase and leaves its pools owed their long routes to the next job. This keeps a stream of frames from starving short routes behind long ones.

A newer frame does not cancel a job for arriving. It cancels a phase's ranking only when it has changed every pool that phase ranks through. Otherwise routes touching changed pools are dropped one by one when they are about to be sized or published, and the changed pools are owed again. Block, epoch, graph replacement and shutdown cancel all of a source's work.

Freshness is checked at several points: before sizing a path, before publishing, and in the state writer when it accepts a result. A candidate whose pools changed is discarded, and if its pools were unchanged but the writer dropped it anyway, `Recheck` returns one of its pools to the next search.

## Coarse ranking

Ranking estimates every affected route with a shallow model and keeps the best few. It has to be fast because a hot pool can sit on hundreds of thousands of routes.

### Shallow model

Each protocol supplies a `pool.RankingModel` (`internal/pool/adapter.go`, `internal/dex/ranking.go`), built once per pool per snapshot. For constant-product pools and for V3-family pools (using virtual reserves at the current price) one leg with input reserve `x`, output reserve `y` and fee `f` in millionths is

```text
out(d) = a·d / (b + c·d)
a = y·(1e6 − f)    b = x·1e6    c = 1e6 − f
```

Two such legs compose into the same form, so a whole route reduces to three integers (`internal/math/cycle_coefficients.go`):

```text
A = a2·a1    B = b2·b1    C = b2·c1 + c2·a1
F(d) = A·d / (B + C·d)
```

The route can profit only if its slope at zero exceeds one, `A > B` (`math.CycleCanProfit`). The profit-maximizing input is

```text
d* = (sqrt(A·B) − B) / C
```

clamped to the funding asset's minimum and maximum input (`math.EstimateCycle`). If `A ≤ B + C·minimum`, the route cannot break even at the minimum input and is skipped without a square root.

For V3-family pools this model is exact only while the swap stays inside the current tick range. Velodrome stable pools build virtual reserves from the curve's slope, and other non-constant-product models only order candidates. Ranking results are never execution evidence.

### Making ranking cheap

- Routes are compiled into directed blocks of up to three hops. Prefix, middle and suffix blocks are shared between routes, and their exact coefficients are cached by pool revision, so a changed pool recomputes only the blocks that contain it (`internal/arb/rank_compiled.go`, `internal/arb/shared_segments.go`).
- A floating-point upper bound encloses the shallow model with outward rounding (`internal/arb/model_bound.go`). With `R` the composed rate and `Q` the composed curvature, profit is at most `min((R−1)·maxInput, (R−1)²/(4·Q))`. A route whose bound cannot beat the current K-th, depth and pool representatives skips the integer estimate. Ties, NaN, overflow and unknown models fall back to the full estimate.
- A per-lane cache keeps route estimates by pool and funding revision, so an unchanged route is not re-estimated.
- Candidate collection, grouping by depth and ranking batches run across the frame's CPU allowance. Batches are committed in a fixed order, so parallel ranking selects exactly what serial ranking would.

### Phantom pools

Some pools rank well and never quote well: a pool whose real fee depends on the priority fee, or a V4 pool with an incompletely loaded tick window. Routes through them would fill the top of every ranking. `internal/arb/phantom_pools.go` counts consecutive non-positive exact quotes per pool. After 512 in a row, the pool leaves coarse ranking for the next 3000 rankings; any positive quote resets its count. A pool that quoted positive recently counts no misses, so a deliberately failing pool cannot push a busy hub out of ranking.

### Selection

Selection keeps at most K candidates per funding asset (`internal/arb/selection.go`, `internal/arb/diverse_selection.go`). Half of K always goes to the highest estimates. The rest protects depths and pools that would otherwise be crowded out by many variants of the same price gap: a bounded quarter for route lengths not yet represented, then routes that add new pools. Equal estimates break ties by pool sequence, so selection is deterministic. Assets are interleaved when candidates are handed to sizing, so one asset's expensive routes do not delay every other asset's first result.

## Amount sizing

Sizing finds the input that maximizes exact gross profit for a selected route (`internal/arb/sizing.go`). Every evaluation quotes every leg with the protocol adapter's integer math.

1. Probe: quote the model's suggested input once. A positive probe is published immediately as an `initial` quote.
2. Grid: quote the minimum, the maximum, 32 points spaced logarithmically between them, the model's suggestion and the asset's notional amount.
3. Refine: take the best grid point and its neighbours as a bracket, then run up to 32 ternary-search steps on it.
4. Finish: quote up to eight consecutive integers at the low end of the final bracket.

The result is published as an `optimized` quote. Only optimized quotes go on to execution.

Three rules keep sizing cheap without changing its result:

- An adapter can declare that its output never decreases with input for a fixed state (`pool.MonotoneQuoter`). For a route made entirely of such pools, `F(right) − left` bounds profit anywhere in `[left, right]`, so a bracket that cannot beat the current best is abandoned. Adapters with amount-dependent hooks or unverified rounding do not declare it.
- A per-job memo reuses a single-pool quote for the same pool, direction and exact integer input (`internal/arb/quote_memo.go`). Errors are never cached.
- Sizing runs in quanta of eight quotes. A route that has not finished goes back in the queue with its bracket and scratch integers, so a long search cannot hold a worker.

### Scheduling

Exact work shares the frame's CPU budget with ranking (`internal/arb/search_cpu.go`). Waiting work gets a freed core in class order: the newest job's short routes first, then earlier jobs' short-route sizing, then everything else. Within the queue, new probes run before continuations, with a bounded burst so continuations still advance; short routes go first within each group; continuations rotate funding assets and favor routes with larger observed profit, with periodic FIFO turns so negative probes still get searched.

A frame's prepared pool and tick data are shared read-only by all workers of a job. The job's snapshot is released only after every worker has returned; cancellation alone does not release it.

### Fast lane

For reactive sending, the runtime can also run a fast lane (`internal/remote/fast_lane.go`). On the main loop, before the full search, it quotes the two- and three-hop routes through a frame's changed pools, a few per funding asset, and hands any opportunity straight to the reactive builder. The full search still runs, and a better intent from it replaces the pending one.

## Exact quoting

Exact quotes use integers only and reproduce each protocol's rounding.

| Protocol | Kind | Math |
|---|---|---|
| Uniswap V2 and compatible pairs | `V2` | Constant product with input fee |
| Uniswap V3, PancakeSwap V3 | `V3` | V3 tick math and swap math |
| Camelot V3 | `CamelotV3` | V3 math over Algebra's `globalState` and tick table, directional fees |
| Aerodrome/Velodrome Slipstream | `Slipstream` | V3 math with a local oracle and dynamic fee model |
| Solidly V3 | `SolidlyV3` | V3 math; fee in `slot0`, changed by a logged `SetFee` |
| Velodrome V2 | `VelodromeV2` | Volatile constant product and the stable `x³y + y³x` curve |
| Uniswap V4, hooks-free, static fee | `V4` | V3 math with the effective fee (LP fee plus the direction's protocol fee), keyed by PoolManager and pool ID |
| MetricOmm (Arbitrum One) | `MetricOmm` | Port of the pool's bin-based swap, anchored to an oracle reference price; extension hooks are not modelled |
| Native wrap | `Wrap` | One-to-one wrap and unwrap |

The V3 swap in `internal/math` follows Solidity's `TickMath`, `SqrtPriceMath`, `SwapMath` and `FullMath` (512-bit intermediate products). It walks initialized ticks inside the loaded window, applying each crossing's `liquidityNet`. A swap that would leave the window fails with `ErrOutOfWindow` instead of assuming no liquidity beyond it; the background tick repair widens the window.

The hot path uses a fixed-width `uint256` implementation (`internal/math/swap256.go`). Whenever an argument, the state or an intermediate value does not fit the protocol's widths (amounts below 2^256, liquidity below 2^128), it hands the swap to the arbitrary-precision implementation, which returns the canonical result or error. Differential tests against the arbitrary-precision path cover every tick price, random swaps and boundary combinations.

### Closed form for two-pool cycles

For an A → B → A cycle within one tick range, `math.ClosedFormTwoLeg` (`internal/math/closedform.go`) solves the optimum exactly. With per-leg input reserves `X`, output reserves `Y` and `c = 1 − fee`:

```text
G(d) = K·d / (M + N·d)
K = c1·c2·Y1·Y2    M = X1·X2    N = c1·(X2 + c2·Y1)
d* = (sqrt(K·M) − M) / N        when K > M
```

It evaluates `floor(d*)` and `floor(d*) + 1` with exact rational arithmetic and returns the better one. The exact quote still decides; the closed form is a starting point. V4 is excluded, because its effective fee is directional.

## Profit and fees

Search measures gross profit in the funding asset: route output minus input. Network fees, loan fees, the quote reserve and tip-dependent pool fees are applied later, in planning (see [execution.md](execution.md#fees)). A route that wins in search can still fail admission there, because fee pricing depends on the chain family, the funding mode and the chosen tip.

## Determinism

Pure functions and state transitions return the same result for the same input. Candidates are ordered by estimate and then by pool sequence; no output depends on map iteration order; workers read immutable snapshots; parallel ranking and selection give the same result as serial.

Online scheduling is the exception. Search runs against wall-clock frames and CPU budgets, so which candidates finish before the next frame can vary. Recall comparisons therefore fix the input, the budgets and the cancellation points (`internal/backtest`, `cmd/routeaudit`).

## Limits

- The catalog and the exploration cache are bounded. A truncated catalog can miss routes; truncation is reported.
- K bounds how many routes get exact sizing per asset. Ranking is an approximation, so the best executable route can fall outside K.
- Routes through unsupported protocols, V4 pools with hooks and quarantined pools are invisible.
- Fees that change without a log are refreshed on a schedule (see [state-and-sync.md](state-and-sync.md#dependencies-outside-logs)), so a quote can use a stale fee until the next read.
