# 4. Deterministic state transitions

## Status

Accepted

## Context

Pool state is replayed in many places: canonical blocks, tentative branches, reorg rollback, snapshot restore, golden replay tests and offline audits. If applying the same logs to the same state could give two results, a replay would not reproduce a live decision and a reorg rollback could leave the store in a state no block produced.

Some pool math depends on time. Slipstream-style dynamic fees, for example, read an oracle window relative to the current block time. A transition that reads the wall clock or a message arrival time gives different fees on replay.

## Decision

- `internal/state` applies events to an in-memory store. It performs no I/O and reads neither the wall clock nor randomness. Results do not depend on map iteration order.
- The store copies on write, so callers never share memory with it. A bounded undo history records each pool's value at the start of a block so a rollback restores exactly that value, including deleting keys that did not exist.
- `internal/ingest` turns receipt logs into state changes. It uses RPC types but does no network I/O.
- Time-dependent math uses block timestamps. A tentative block, whose header is not yet known, uses its parent's timestamp. Arrival times of stream messages are recorded for latency analysis and never enter a transition.
- Anything that needs outside data (fee refreshes, tick windows, pool discovery) is read outside the transition and passed in as data with the block it belongs to.
- `internal/domain` holds shared types and invariants only, with no I/O.
- Operator controls such as the delivery gate are read outside transitions and only decide whether a result leaves the process.

## Consequences

- Golden replays and recorded fixtures reproduce live state exactly, so state and quote regressions are testable offline.
- A reorg or a failed apply can be undone without re-reading the chain.
- Callers must thread time and external reads explicitly, which adds parameters to code paths that would otherwise be shorter.
- A tentative block's timestamp can trail the real header by up to a block interval. For oracle windows of minutes this is negligible; a protocol whose math is sensitive at that scale would need the real timestamp before its quotes are trusted.
