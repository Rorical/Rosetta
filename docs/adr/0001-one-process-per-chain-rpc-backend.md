# 1. One process per chain on an RPC backend

## Status

Accepted

## Context

Rosetta searches and executes cyclic arbitrage on L2 chains. The chains differ in fee charging, transaction ordering, pre-confirmation streams and conditional-transaction semantics, so a single process serving several chains would have to branch on all of them at once.

An earlier design embedded a Nitro node in the process to replay feed transactions locally and confirm plans against its own state. It required pinned Nitro and go-ethereum sources, CGO, static libraries and a Go workspace, and `make check` could not run without a local Nitro checkout. Its execution database, write-ahead log, trie garbage collection and L1 input recovery took most of the code and test cost. An Ethereum L1 execution target was also considered. Neither reached production use.

## Decision

- A process binds to exactly one L2 chain for its lifetime. `cmd/rosetta` injects the backend factory from `internal/remote`; `internal/bot` checks chain identity and runs the lifecycle and holds no RPC, feed or storage logic.
- Every chain uses the RPC backend in `internal/remote`. Blocks and receipts come from JSON-RPC, pre-confirmation data from WebSocket streams (the Arbitrum sequencer feed and Fast Feed, OP Stack Flashblocks), and full confirmation from `eth_call` and tracing at a pinned block hash.
- There is no in-process node and no L1 execution backend. `go.mod` does not depend on Nitro, and the feed decoder in `internal/feed` is written from the public wire format without Nitro source.
- Production builds use `CGO_ENABLED=0`. Rust appears only in tests (the OP reference engine).

## Consequences

- The build is one static binary, and one `make check` covers the repository without external checkouts.
- Running several chains means several processes, each with its own flags, journals, keys and run directory. A cross-instance view needs a separate process (the console).
- Latency is bounded by stream delivery and RPC round trips. There is no local re-execution of feed transactions, so state between blocks comes from receipts and logs carried by the stream.
- Full confirmation depends on the provider's tracing and state-override support. A provider without it can still serve search and reactive execution, but cannot produce ready plans.
- Reintroducing an embedded node or an L1 backend is out of scope; it would bring back the build and maintenance cost this decision removed.
