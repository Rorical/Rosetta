# 3. Canonical state from whole blocks, speculative branches for pre-confirmation data

## Status

Accepted

## Context

Pre-confirmation streams (OP Stack Flashblocks, Arbitrum Fast Feed) deliver transactions and receipts hundreds of milliseconds before a block is readable over RPC. Most opportunities live in that window. The streams can also skip frames, reconnect, contradict themselves or be reorganized, and a partial block has no block hash.

Reading every block over RPC is slow and costs a header and a receipts call per block. Searching or executing against state that mixes confirmed blocks with unproven fragments produces quotes that are wrong in ways nothing downstream can detect.

## Decision

The pool store has one canonical state and any number of tentative branches over it.

Canonical state advances only by whole blocks whose hash is known and whose parent is the accepted head. A block reaches it in one of two ways:

- Over RPC: the block and `eth_getBlockReceipts`, checked for transaction hashes, indexes, status and log order, with the canonical hash re-read.
- Sealed from a stream: a block is complete when the next block's first frame names its hash as parent. On OP Stack the Flashblocks frames carry the receipts. On Arbitrum One, Fast Feed receipts are joined with the block hash and header from the standard feed when the Fast Feed carries whole blocks.

Sealed blocks are trusted on probation. After start and after any mismatch, each of the next 30 is compared with RPC receipts; after that, one block per interval is. A gap, a replaced block, a missing parent, a reconnect or receipts the stream did not carry send that block to the RPC path. Unverified stream blocks are kept; on a mismatch they are re-checked by hash, and all pools are reloaded only if one differs.

Partial blocks apply to a tentative branch layered over canonical state. A branch never writes canonical state. It is dropped on a gap, contradiction, reconnect or timeout, and reconciled when canonical state reaches its block. A synthetic prefix identifier is never treated as a block hash, and a tentative result is never paired with an unrelated canonical snapshot to produce a ready plan.

Startup loads pools from an explicit pool list, a token and factory catalog resolved at one block hash, or a snapshot anchored to a block hash. The snapshot is replaced atomically after bootstrap and at shutdown. A reorg reloads the known pools; a follower too far behind reseeds at the tip.

## Consequences

- Search runs on the freshest stream data without that data leaking into the state every later block builds on.
- Between verifications the content of a sealed block is the stream's word. A wrong stream block is caught at the next check, and every block after it is re-checked.
- Steady-state RPC load falls to periodic checks, head reads and fee reads for the engine's own transactions.
- Without a complete stream, canonical latency is the RPC latency and tentative branches carry only what the stream provides.
