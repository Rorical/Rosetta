# 9. Funding modes

## Status

Accepted

## Context

A route needs principal in its starting asset for the first leg. The source of that principal changes what the transaction costs and what it can lose. A standalone flash loan adds a fee and roughly two extra transfers and a callback. A wallet needs balance and allowance and pays two transfers. Some pools pay a swap's output before collecting its input, which lends the principal for free inside the swap. A route that stays inside one V4 PoolManager can settle only its net delta.

Gas spent on funding is paid on every attempt, including lost races, so the cheapest available mode decides whether small opportunities clear their execution budget at all.

## Decision

The executor supports five funding modes, and the planner picks one per intent:

- `wallet`: the operator's balance and allowance (or `msg.value` for native currency on V4 routes). Principal is returned in the same call.
- `v3_flash`: a standalone V3 `flash` from a lender pool chosen by balance. The fee comes from the callback and is capped by `maxLoanFee`. The lender cannot also appear as a leg.
- `v3_flash_swap`: the first leg is a V3-family pool (Uniswap, Pancake, Slipstream or Solidly V3). It delivers output first, the callback runs the rest of the route, and the first pool is paid from the route's own output. No loan fee and no principal transfer.
- `v4_flash`: every leg is on one V4 PoolManager. One `unlock` runs all swaps; only the net profit is taken.
- `inventory`: the executor's own balance of the asset. No transfer in or out; the final check requires the balance to grow by at least `minProfit`.

`planning.ChooseFunding` subtracts each available mode's total cost (execution budget plus loan fee) from the route's gross and picks the highest net. When nets tie, callback-style and inventory modes win over wallet, and wallet wins over a standalone loan. A nil cost means missing evidence and excludes the mode. If a cycle can only be funded by a standalone loan but has a later V3-family leg entered by a configured asset, the planner also rotates the cycle to start at that leg and quotes it as a separate candidate.

For V3-family routes that run under conditions (ADR 0013), a chained hot path (`executeHot`) skips the executor in the middle of the route: the outermost callback asks each earlier pool for exactly what is owed and sends it straight to the pool owed it. Intermediate tokens never reach the executor, and only the asset's profit is checked at the end. Its senders are fixed at deployment. Routes that do not fit its constraints (each pool once, at least one callback pool, quoted amounts within the encoding) use the packed entry point.

## Consequences

- Most routes need no standing capital and pay no loan fee.
- Inventory mode needs funds parked in the executor, which raises the exposure of a contract that already holds profit until it is swept.
- The chained hot path has no intermediate balance or per-leg minimum-output checks. V3-family legs carry price bounds (ADR 0012); V2 and oracle-priced legs have none, so a route takes this path only when those legs are held by conditions. The pause flag is enforced through a condition on the executor's slot 0, so a plain transaction to `executeHot` cannot be paused, and removing its senders takes a redeployment.
- Each mode is a separate success and revert gas profile, so gas estimates and bid history are kept per funding mode.
