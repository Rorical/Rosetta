# 15. Bidding by expected profit over a win-probability model

## Status

Accepted

## Context

On priority-ordered chains the winner of a contested opportunity is usually the highest tip in the same pre-confirmation frame. Tipping a fixed share of the profit loses to rivals that bid more on large opportunities and overpays on small, uncontested ones. A losing send also has a cost that depends on how it is sent: free when a condition drops it (ADR 0013), a few tens of thousands of gas at a price bound (ADR 0012), or most of the route at the profit check.

Rival behaviour is observable. The stream shows every transaction that touched our route's pools, its tip and the contract it called, and our own settlements show which tip would have won.

## Decision

- `bid_model` is `shadow` (default: compute and record, sign with the configured share tip), `apply` (sign the model's tip) or `off`. On arrival-ordered chains the tip is zero or the configured minimum.
- The win probability at tip `t` is `(1 − q) · Π_i (1 − π_i · P(b_i ≥ t)) · (1 − π_u · P(b_G ≥ t))`: `q` is the chance a rival lands first regardless of tip, `π_i` the presence of rival `i` and `b_i` its bid, with an unknown-rival term on the global distribution.
  - Rivals are identified by the contract they call, since one bot often sends from many addresses.
  - Bids are modelled as a share of the opportunity's value, and presence is weighted by value, so a rival that contests only large opportunities is not diluted by small ones. Estimates shrink from route to rival to global to prior.
  - Windows are kept for two hours with an hourly half-life, and history survives restarts.
- The raw probability is calibrated on the logit scale against our own outcomes, with terms for opportunity size, decision age and lane under a ridge penalty. Our settled sends also record the lowest tip that would have won, and conditional drops count as losses to the highest rival tip seen on the held pools.
- The chosen tip maximizes `P(t) · (surplus − t · gas) − (1 − P(t)) · loss(t)` over rival bid breakpoints and a floor, subject to `max_priority_fee_per_gas`, `max_fee_per_gas`, `max_transaction_fee` and the surplus per unit of gas limit. A tip below `bid_min_win_probability` (default 0.2; 0 disables) is not sent.
- `loss(t)` follows the route's sending shape: zero when every pool is held by a condition, the precheck's revert gas when the route carries price bounds, otherwise about half the success gas. Conditional sends learn their loss cost in a separate history.
- Floors and ceilings handle cases the model cannot see:
  - When a loss is free, the tip is at least a fixed share of the opportunity's value, larger for smaller opportunities.
  - A route's floor is the median share rivals paid in recent contested windows on its pools.
  - A route that has never run end to end, or has a leg neither held nor bounded, is capped at a small multiple of the base fee. A proven, bounded route above a size threshold may bid half its value per gas.
  - Rivals that often bid above value are flagged; the model then sends at most an occasional zero-tip probe against them.

## Consequences

- Bids scale with competition, and sends whose expected profit is negative are skipped and recorded with a reason.
- The model learns only from opportunities it sends. A size range it stops bidding on stops producing outcomes, and its correction decays slowly.
- A revert caused by our own quote error looks like a lost auction to the calibration and pulls the win probability down.
- The constants in the floors come from replays of past contests and need re-measurement on a new chain.
