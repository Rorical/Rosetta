# 17. Token risk registry and pool quarantine without allowlists

## Status

Accepted

## Context

The executor trades any pool the engine discovers (ADR 0008), and discovery reaches tokens nobody reviewed. Some tokens tax transfers, so the executor receives less than the pool sent and the route reverts at the next balance check. Others refuse transfers to contracts they do not recognise. Some pools quote one thing and execute another. Each bad token or pool can produce a stream of reverting sends, each paying gas.

An allowlist of tokens and pools would stop this but also blocks most of the market: early shadow runs rejected most opportunities because a route crossed a token outside a short configured list. Fixing taxed quotes with an estimated tax rate is fragile, since the rate can change at any time.

## Decision

Only start assets are configured, each with a principal cap (ADR 0018). Intermediate tokens and pools are bounded by the discovery scope (protocols, factories and optional quote tokens) and by evidence collected at runtime:

- `internal/tokenrisk` keeps a chain-bound, persistent registry (`-token-risk-file`) of tokens with demonstrated transfer problems. A token is recorded on concrete evidence only:
  - a shortfall: a trace shows the executor reading its own balance around exactly one incoming transfer that delivered less than was sent, or a canonical receipt shows a pool's swap sending more than its recipient received;
  - a refusal: a route pool's plain revert while paying the executor, at a block where the payer held the amount. Reverts reporting an insufficient balance do not count, since a pool can move its own balance within the transaction.
  Start assets and wrapped native are never recorded automatically, so an attacker who stages a failing transfer can at most close routes through one intermediate token.
- Discovery can reject pools whose tokens' bytecode contains configured 4-byte selectors (`deny_token_code_selectors`), catching families of trap tokens deployed from one template.
- Optional preflight (`preflight_simulation`) calls the deployed executor at the quote block for each new route. A route that passes stays passed until one of its transactions reverts on-chain; a revert is diagnosed off the send path and may record a token.
- `internal/poolrisk` quarantines a pool (`-pool-risk-file`) after three reverts across at least two distinct routes executed at the quote state, counting only pools that have never succeeded. A pool that has succeeded is never blamed, so decoy pools cannot drag a healthy pool into quarantine. Probes run after the quote state do not count. Quarantine is sticky; removing an entry is an operator action.
- Short holds cover the time before evidence settles: when our own send reverts and no other transaction touched the route's pools after the quote, those pools are skipped until they change or a minute passes; after a conditional rejection, for up to two seconds.
- Both registries are checked at graph build, ranking, exact quoting, planning, reactive admission and again by the dispatcher before signing and before broadcast.

## Consequences

- The engine can trade the whole discovered market, and the first revert on a bad token usually closes every route through it.
- Absence from the registry certifies nothing. The contract's balance and profit checks stay mandatory.
- A pool that turns bad after succeeding once is not quarantined automatically.
- Quarantine thresholds were not calibrated on historical reverts; false positives and misses are measured from the quarantine log against route audits.
