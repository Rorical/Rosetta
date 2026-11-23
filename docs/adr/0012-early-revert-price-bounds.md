# 12. Early revert on moved pools

## Status

Accepted

## Context

On a priority-ordered chain, several bots see the same opportunity and race for it. The loser's transaction still executes. Without any early check it runs every swap and fails at the final profit check, burning most of the route's gas at the tip it bid. In contract tests on a mainnet fork, a lost two- to four-leg route burned 180,000 to 450,000 gas this way.

A rival that takes the opportunity first moves at least one of the route's pools against it. Reading those pools before doing anything else detects most lost races for the cost of a few storage reads.

## Decision

- Packed plans may carry a one-sided price bound per leg. Before funding and before the first swap, the executor reads each bounded pool and reverts with `PoolMoved(step)` if its price has moved against the leg past the bound, or `UnreadablePool(step)` if the read fails.
  - V3-family pools: the sqrt price from `slot0` (`globalState` for Algebra).
  - V4: the pool's slot0 word through `PoolManager.extsload`.
  - V2 and Velodrome volatile pools: the reserve ratio from `getReserves`.
  - Favourable moves always pass.
- The off-chain planner sets each bound where that pool alone would consume the route's whole margin: the price at which the leg's rate falls by `1 − required / output`, with `required` being principal plus `minProfit` plus the loan fee cap. Such a transaction would fail the profit check anyway.
- A cumulative variant takes each leg's reference price and multiplies the legs' rate changes, so several pools that each moved a little fail together. A favourable move offsets an adverse one up to a cap.
- Velodrome stable pools get no bound, since their reserve ratio moves far more than their rate.
- On the chained hot path the pool that triggered the opportunity always carries a bound, because a condition cannot hold a pool that just changed (ADR 0013).
- `price_precheck` and `price_precheck_cumulative` enable the checks per deployment; both need an executor that decodes the matching packed version.

## Consequences

- In the same fork tests a lost race reverts after 18,000 to 31,000 gas, and a successful route pays about 2,000 extra gas per bounded pool.
- The bid model prices a lost race at the precheck's revert gas instead of half the route, which raises the tip it can afford.
- Movement spread over several pools that stays inside each per-leg bound still reaches the profit check. The cumulative mode catches that case.
- A per-leg bound can reject a transaction that would have succeeded when another pool on the route moved favourably at the same time; the cumulative mode credits that move. The false-rejection rate is measured from revert reasons.
