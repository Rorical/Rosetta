# 5. Protocol adapters and pool identity

## Status

Accepted

## Context

The engine prices routes across several DEX families: Uniswap V2 and its forks, Uniswap V3 and its forks (Pancake, Solidly V3), Algebra (Camelot V3), Slipstream, Velodrome V2 stable and volatile pools, hookless Uniswap V4, an oracle-priced bin market maker, and wrapped native as a 1:1 leg. Each has its own state layout, events, fee rules and callback.

A contract address does not identify a pool in every family. All Uniswap V4 pools live inside one PoolManager, so keying state by address made sibling V4 pools overwrite each other. Several forks also share a swap ABI while differing in fee storage or callback name, so an address plus a guess at the protocol was not enough to quote or execute.

## Decision

- A pool's identity is `domain.PoolKey`: address, kind, V4 pool ID and V4 hooks address. It is comparable, and its sort order is the canonical order for dirty sets, snapshots, the graph and state deltas.
- `domain.PoolKind` names the protocol family. Its numeric values match the executor contract's leg kinds, so a key maps to an execution branch without a lookup table.
- `internal/pool` defines the `Adapter` contract: quote exact input against a state and deep state without mutating either. Adapters may also expose a ranking model for coarse screening and certify that their quotes are monotone in input. Only adapters that can prove monotonicity opt in.
- Protocol math and decoding live in `internal/dex/<protocol>`. The `internal/dex` root registers protocols and holds shared event decoding. Forks that share V3 math depend on `internal/dex/uniswapv3`.
- Logs route to pools by address, and V4 logs by PoolManager address plus the pool ID topic.
- V4 support is limited to pools without hooks and with static fees. Hooked pools are not discovered, and the executor builds V4 pool keys with a zero hooks address.
- New protocols are added as an adapter, a state transition, an executor leg and fixtures from real chain data, together.

## Consequences

- Search, state and execution handle all families through one key type and one quote interface.
- Pancake V3 shares the V3 pool kind with Uniswap but stores its fields in different slots. Code that reads raw storage (conditional transactions) discovers layouts per pool instead of per kind.
- Hooked V4 pools, Curve, Balancer and other unsupported families are invisible to search. Opportunities that pass through them are missed.
- An adapter whose quotes diverge from the chain produces reverts that the pool quarantine (ADR 0017) eventually isolates. Real-chain fixtures are the first line of defence.
