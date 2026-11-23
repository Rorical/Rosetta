# 6. Staged search over a precomputed cycle table

## Status

Accepted

## Context

A pool graph with a few hundred to a few thousand pools has millions of simple cycles of length four or less. An opportunity typically survives for a block or two, and each stream frame changes only a handful of pools. Quoting every cycle exactly on every frame is impossible within that window, and enumerating cycles on the hot path is too slow.

Exact quotes for concentrated-liquidity pools need tick state, which is expensive to load and copy. Cheap approximations work for ordering candidates but overestimate once a swap crosses a tick.

## Decision

- `internal/graph` precomputes a table of simple cycles with an inverted index from pool to cycles. The table is filled shortest length first; a length that does not fit is seeded per pool and direction so every new pool has at least some cycles. Table size is set in the search config, or `auto`, which takes the smaller of a memory-derived and a per-core limit.
- Discovery adds pools in the background and extends the table from the new pools' edges, keeping existing cycle IDs. Reorgs and resets rebuild it.
- Each frame marks the pools it changed. Only cycles through those pools are searched.
- Search runs in two stages:
  - Rank: a coarse integer valuation of every dirty cycle using each protocol's ranking model, keeping a bounded top K per starting asset in a heap. Assets are never compared in raw units.
  - Exact: only selected cycles load tick windows and run exact adapter quotes with amount optimization. Two-pool cycles get a constant-liquidity closed-form input as a sizing hint, and the exact quote decides.
- Short routes are quoted before long ones, and a route's first quote is published before its sizing finishes. A new frame cancels a search only when it changes the pools that search depends on.
- Ranking and exact work run on all cores under a shared CPU budget. Parallel stages commit results in the same order a single thread would, so output is reproducible.

## Consequences

- Latency from a frame to a quote depends on how many cycles touch the dirty pools, and stays in milliseconds for short routes on large tables.
- Coarse ranking and the top-K cutoff trade recall for speed. A cycle the ranking model undervalues is never quoted exactly, so ranking errors show up as missed opportunities, which offline audits (`cmd/routeaudit`) measure.
- The table holds cycles only up to the configured depth. Longer routes are out of reach regardless of value.
- The table's memory grows with pool count and depth. `auto` sizing ties it to the host, so two hosts with different limits can search different cycle sets.
