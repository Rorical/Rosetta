# 10. Ready plans and reactive intents

## Status

Accepted

## Context

There are two ways to decide that a route is worth sending.

The first is full confirmation: run the exact unsigned executor call against a pinned state (a canonical block hash, or a pre-confirmation prefix replayed or exported at that hash), trace it, and prove principal, profit, gas, fees and nonce from the trace. This takes one or more trace round trips and requires a provider that supports tracing and state overrides.

The second is to act on the local quote alone. An opportunity seen in a pre-confirmation frame usually lasts a block or less, and a trace round trip often outlasts it. The executor's on-chain checks (ADRs 0007 and 0008) already bound the loss of a wrong quote to gas.

Mixing the two would let an unconfirmed route borrow the authority of a confirmed one.

## Decision

- `exec.ReadyPlan` is the output of full confirmation. It carries the exact invocation and its evidence (state source, fee evidence for the chain family, executor code identity). Its ID is a digest of its contents including chain identity, and a consumer must call `ValidateForDispatch` against current state immediately before use. Canonical and tentative confirmations run in separate latest-only queues, and a newer task retires an older one.
- `exec.ReactiveIntent` is explicitly unconfirmed. It carries the exact unsigned route, a conservative cost budget, the bid decision and the contract guards (per-leg minimums, price bounds, `minProfit`, deadline). It never carries EVM evidence or ready status. The two types do not convert into each other.
- Reactive intents are built off the main loop from cached context (account balances, fee parameters, executor configuration) that is refreshed in the background. The send path waits for no trace, balance read or fee read.
- An intent is admitted only if the cached context is within `context_max_age_ms`, the route's pools are unchanged since the quote, funding and the full fee budget fit, and the net after costs clears `minimum_net_profit`. A newer, better quote replaces a queued intent before broadcast.
- Optional preflight simulation (`preflight_simulation`) runs each new route once as an `eth_call` of the deployed executor at the quote block. A route that passes is remembered until one of its transactions reverts on-chain.
- Reactive mode is `shadow` or `live`. Shadow builds, prices and records intents without signing; live signs and sends through the dispatcher (ADR 0019). Execution paper runs selected intents against the real contract at the quote state to measure what would have happened.

## Consequences

- Ready plans are slow and carry proof; reactive intents are fast and carry only guards. The dispatcher signs and sends only reactive intents. Ready plans are written to `-plan-out` for external consumers and audits.
- A reactive send can revert on-chain. Its cost is bounded by the fee budget and reduced by price bounds and conditions (ADRs 0012 and 0013).
- The quality of reactive decisions depends on the cached context being fresh, so context age is a hard admission limit.
