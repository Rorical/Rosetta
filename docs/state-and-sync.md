# State and synchronization

Rosetta keeps an in-memory model of the pools it trades through. Canonical state follows the chain's accepted blocks. Speculative branches apply transactions that a sequencer stream has published but the chain has not yet sealed. This document describes who owns that state, how each branch advances, and what happens on gaps, conflicts and reorgs.

The code lives in `internal/state` (the store), `internal/ingest` (applying receipt logs) and `internal/remote` (the follower, stream handling and the state writer).

## The pool store

`state.Store` (`internal/state/store.go`) holds pool state and the chain head. It is not an EVM database: it has no accounts, code or general storage, only what the protocol adapters need to quote.

| State | Contents |
|---|---|
| Block identity | Number, hash, parent hash and timestamp of the head |
| Shallow | For every tracked pool: tokens, price or reserves, active liquidity, fee and protocol metadata |
| Deep | Tick bitmap words and per-tick liquidity inside a loaded window around the current tick |
| Protocol models | Slipstream oracle observations and dynamic-fee parameters; Velodrome stable flags and decimal scales; MetricOmm registration |
| Markers | Dirty pools (changed since the last search) and stale pools (state known to need a reload) |

A pool is identified by `domain.PoolKey{Address, Kind, PoolID, Hooks}` (`internal/domain/types.go`). For Uniswap V4 the address is the PoolManager singleton, so the pool ID is what separates pools; matching by address alone would treat every V4 pool as one. `PoolID` and `Hooks` are zero for every other protocol.

### Ownership

One goroutine, the state writer, owns each store. `Store` is not safe for concurrent writes. Readers take a `state.View` (`internal/state/view.go`), an immutable view captured on the writer goroutine. Pool and tick values are copy-on-write: a write installs a new value, so a view keeps seeing the state it was captured at while the writer moves on. Getters return copies.

Speculative branches use `Store.Fork`, which copies only the top-level maps and markers. Each fork has its own dirty markers and undo history and shares pool values with its parent until one side writes. This keeps a branch cheap to create on every reconnect or rebase.

### Determinism

State transitions must give the same result for the same input:

- `internal/state` reads neither the wall clock nor randomness, and imports neither `time` nor `math/rand`.
- No result depends on map iteration order. Snapshots sort pools, ticks, bitmap words and markers before encoding.
- Each transaction's logs are applied as one unit (below), so no reader sees half of a transaction.

Determinism is what makes `internal/backtest` meaningful: a recorded block replayed through the same store and search must produce the same candidates.

### Undo

The store keeps a bounded multi-block undo history (`maxUndoBlocks = 8` in `internal/state/undo.go`). Each block is a frame opened with `BeginBlock` and closed with `Commit`; `Rollback` discards the open frame. The RPC follower does not use multi-block rollback for reorgs; it reseeds instead (see [Reorgs](#reorgs-and-resets)).

## Applying receipts

`internal/ingest` turns receipt logs into store writes. It performs no network I/O and does not decode batches or signed transactions; it consumes receipts that the backend has already fetched.

A canonical block is staged as a `TransactionBranch` over the accepted store (`internal/ingest/transactions.go`):

- Transactions apply in index order. Each opens its own undo frame; if any log fails to apply, the whole transaction rolls back and the error is returned.
- A reverted transaction keeps its index but has no logs to apply. A failed transaction that carries logs is rejected as malformed.
- `Commit` requires the exact block reference and the ordered list of transaction hashes, so a block with a missing transaction cannot be committed.

Logs from pools the store does not track are ignored. New pools enter through discovery (below), never through an unknown log.

## Canonical follower

The follower (`internal/remote/source.go`) reads blocks and their receipts by number from the RPC endpoint. A block is accepted only if its parent hash is the current head and its receipts are complete. After reading receipts at the tip, the follower checks the tip block again, so a block replaced while its receipts were being read is not applied.

What wakes the follower depends on the event source:

| Source | Wake-up | Block contents |
|---|---|---|
| `rpc` | Polling (default every 2 s), or a heads subscription | RPC |
| `sequencer` (Nitro) | Each standard sequencer feed message | RPC |
| `flashblocks` (OP Stack) | The Flashblocks stream | Sealed from the stream, sampled by RPC |
| `sequencer` with Fast Feed and canonical first index 0 (Arbitrum One) | Standard feed and Fast Feed | Sealed from the streams, sampled by RPC |

### Stream-sealed blocks

When a stream carries every transaction of a block with full receipts, waiting for RPC on every block only adds latency. Two modes seal canonical blocks from streams:

- OP Flashblocks: the index-0 flashblock of block N names its parent, which seals block N−1 (`internal/remote/flash_canonical.go`).
- Arbitrum One: `fastSealer` (`internal/remote/fast_seal.go`) assembles a block from Fast Feed receipts once the next block's first transaction arrives, and takes the block hash, timestamp and L1 block from the standard feed.

A sealed block is applied only if its parent is the accepted head. Its contents are trusted until the next verification:

- A new follower, and one that has just found a mismatch, verifies each of its first 30 blocks against RPC (`flashVerifyEvery`).
- After that it verifies one block per minute of chain time (`flashVerifyInterval`), converted to blocks for the chain.
- If no seal arrives for 6 s (`flashSealStale`), the follower returns to RPC head polling.
- A verification mismatch compares the unverified sealed blocks against RPC; any difference triggers a reseed.

This trades per-block RPC verification for latency. The sampling rate bounds how long a stream error can persist in canonical state before it is found.

### Catch-up and gaps

A follower behind the tip catches up block by block. If it falls more than 30 minutes of blocks behind (`catchUpLimit`), it reseeds at the tip instead of replaying the gap. During catch-up the writer applies blocks but defers search and historical dependency reads, which would compete with the follower for RPC capacity. Back at the tip, it searches once over the pools that changed while it was behind.

### Reorgs and resets

A parent-hash mismatch, a failed stream verification or a canonical snapshot that is no longer canonical leads to a reseed (`internal/remote/bootstrap.go`). The follower reads every known pool again at a fresh tip, using reads pinned to that block hash (EIP-1898 `blockHash` with `requireCanonical`), and checks that the tip is still canonical afterwards. The state writer then replaces the store and, when discovery and search are enabled, rebuilds the route topology. Speculative branches, pending confirmations and candidates tied to the old state are retired.

Reorg detection is limited to what the follower observes. It is not a finality check.

## Speculative branches

Speculative input never writes canonical state. It updates a branch that sits on top of the canonical store and is rebuilt against each new canonical block.

### Arbitrum Fast Feed

The Fast Feed publishes each transaction with a partial receipt during the priority-fee auction round, before the block closes. It has no replay and no final block hash, and inclusion is not guaranteed.

`ingest.FastBranch` (`internal/ingest/fast.go`) applies those receipts in feed order. It is bounded (4096 transactions or 32 MiB by default). Positions are deduplicated by `(block, transaction index)` and a digest of the transaction; a conflicting duplicate resets the branch. A gap in positions also resets it, and the branch resumes at the next block's first transaction.

Feed positions are not block indexes. `-fast-first-index` and `-fast-canonical-first-index` give the measured offsets, and `cmd/fastprobe` measures them for a chain. Both default to −1, which the backend rejects when a Fast Feed is configured.

When a canonical block arrives, `FastBranch.Reconcile` (`internal/ingest/fast_reconcile.go`) installs the verified block as the new base, compares transaction digests with the canonical receipts and replays the suffix that is not yet in a block. A mismatch, or a system transaction before the mapped first index that touches a tracked pool, returns `ErrFastConflict` and the branch restarts from the new base.

The client (`internal/feed/fast_client.go`) invalidates the branch before every connection and after every disconnect, because a block in progress may have lost transactions. Authentication and rate-limit errors stop the client; other failures reconnect with backoff. If the Fast Feed has been silent for 2 s (`fastQuiet`), standard blocks are searched again.

### OP Flashblocks

Flashblocks arrive as `(payload, index)` frames. The runtime checks continuity: the same payload and index with a different digest, or a skipped index, resets the branch. Each frame's receipts update a log branch (`ingest.LogBranch`, `internal/ingest/log_branch.go`).

Streams that omit receipts are rejected at startup (`internal/remote/preflight.go`) and at runtime, because a transaction list alone cannot update pool state. `-flashblocks-receipts-rpc` opts in to fetching missing receipts by RPC; until all receipts of a frame arrive, the frame does not touch pool state.

On first attach and after a gap, the branch needs a canonical parent. While it waits, frames are buffered up to 256 frames or 32 MiB (`waitForFlashParent` in `internal/remote/flash_chain.go`); a discontinuity in the buffer resets it. At startup the preflight waits up to 2 s for the RPC to serve the stream's parent block.

`LogBranch.Reconcile` (`internal/ingest/log_reconcile.go`) compares the branch with each canonical block. A seal or parent mismatch resets the branch. A missing or changed receipt tail is repaired from canonical state, and later frames are rebased onto the new base.

Synthetic prefix identifiers on a speculative branch are not block hashes and are never used as one.

## Dependencies outside logs

Some state changes without a log, or needs more data than logs carry:

| Dependency | Handling |
|---|---|
| Tick windows | A quote that leaves the loaded window fails with an explicit error. `internal/remote/tick_repair.go` reloads canonical windows in the background and replays the retained receipt tail (up to 1024 updates or 32 MiB) before adopting them. Unknown depth is never treated as zero liquidity |
| Slipstream dynamic fees | A local model follows the oracle and fee rules (`internal/state/slipstream.go`). A configuration change marks the pool for a reload. The fee depends on the transaction's priority fee, so execution reprices it at the chosen tip |
| Velodrome V2, Camelot V3 and hooks-free V4 fees | Re-read at a pinned block once per hour of chain time (`canonicalFeeInterval` in `internal/remote/fee_schedule.go`), and immediately after a fee-changing log. A fee change without a log can go unseen for up to that interval |
| OP L1 and operator fees | Parsed from the L1 attributes deposit and the GasPriceOracle; re-read when the fee parameters change |
| Nitro L1 price | ArbGasInfo `getPricesInWei`, reused for 60 s |

## Pool discovery

`internal/discovery` scans factory and PoolManager creation logs, filters candidates by tokens, fee, hooks and active liquidity, and ranks them by depth. `internal/catalog` verifies a configured token and factory universe at a pinned block hash, checking each pool's address against its factory and its own metadata. Both run in the background. A new pool reaches the store only after its state has been loaded at a pinned block and the receipts since then have been replayed, so the writer adopts it at the current head.

## Snapshots and persistence

Pool snapshots use deterministic JSON (`internal/state/snapshotcodec.go`). The encoder writes the lowest format version that can hold every pool, and the decoder rejects unknown versions so an older binary cannot silently drop newer state.

| Version | Adds |
|---|---|
| 1 | Base shallow and deep pool state |
| 2 | Camelot V3 directional (reverse) fee |
| 3 | Protocol metadata for non-Uniswap curves (Velodrome factory, stable flag, decimal scales) |
| 4 | Slipstream oracle and block fee cache |
| 5 | MetricOmm registration |

`-pool-snapshot` resumes from a snapshot only if its block is still canonical on the configured RPC; otherwise startup fails. The runtime writes a snapshot on shutdown through a temporary file and rename.

Other records persist independently and are never rolled back with pool state:

| Record | Package | Notes |
|---|---|---|
| Dispatch journal | `internal/exec/dispatch_store.go` | Nonces, every signed variant, fee reservations, settlements. Written atomically with fsync under a file lock |
| Token risk evidence | `internal/tokenrisk` | Chain-bound, append-only JSONL. Evidence is sticky across reorgs, because it records observed behavior |
| Pool quarantine | `internal/poolrisk` | Same format; removing an entry is an operator action |
| Bid estimator history | `internal/remote/bid_store.go` | Saved periodically and on shutdown |
| Run output | `-candidate-out`, research and reactive output | Large; kept out of git |

## Backpressure

State input is never dropped to keep up. Canonical updates and stream frames use blocking queues, and the follower waits for the writer to accept each block. The one exception is the Flashblocks seal queue: when it is full, a seal is dropped and that block is read by RPC instead.

Search may cancel or merge work, and carries the dirty set it was given forward to the next job. A snapshot or reader is released only after every worker using it has returned.

Observation queues are allowed to drop samples and count the drops: competition records, research samples and diagnostics. Execution observations are not: if that queue is full the instance stops. If the dispatch journal cannot be written, the dispatcher refuses every later send until restart.

## Invariants

1. A transaction's receipt logs apply as one unit; no view shows half a transaction.
2. Canonical state advances only from complete blocks whose parent is the accepted head. Speculative identities stay separate from canonical ones, and missing evidence is never filled in.
3. Quotes, funding, pool fees and fee budgets are bound to an explicit state and version.
4. Restoring a snapshot does not clear unknown sends or persisted risk evidence.
5. Reorg detection covers only what the follower observed.
