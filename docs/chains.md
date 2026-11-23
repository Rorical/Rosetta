# Chains

Rosetta runs against L2 chains of two families: Arbitrum Nitro and the OP Stack. One process follows one chain. The family, not the chain ID, selects execution and fee semantics, and a chain ID never borrows another family's rules.

How to select a chain and override its endpoints is in [configuration.md](configuration.md#network-and-inputs).

## Supported presets

| `-network` | Chain ID | Family | Parent chain | Ordering | Nominal block time |
|---|---:|---|---|---|---:|
| `arbitrum-one` (default) | 42161 | `nitro` | Ethereum (1) | priority | 250 ms |
| `robinhood` | 4663 | `nitro` | Ethereum (1) | arrival | 100 ms |
| `robinhood-testnet` | 46630 | `nitro` | Sepolia (11155111) | arrival (`UNVERIFIED`) | 250 ms |
| `optimism` | 10 | `op-stack` | Ethereum (1) | priority | 2 s |
| `base` | 8453 | `op-stack` | Ethereum (1) | priority | 2 s |
| `unichain` | 130 | `op-stack` | Ethereum (1) | priority | 1 s |
| `ink` | 57073 | `op-stack` | Ethereum (1) | priority | 1 s |

Default public endpoints from `internal/config/network.go` and `internal/config/op_network.go`:

| Preset | RPC | Sequencer feed | Flashblocks | Submit |
|---|---|---|---|---|
| `arbitrum-one` | `https://arb1.arbitrum.io/rpc` | `wss://arb1-feed.arbitrum.io/feed` | | |
| `robinhood` | `https://rpc.mainnet.chain.robinhood.com` | `wss://feed.mainnet.chain.robinhood.com` | | |
| `robinhood-testnet` | `https://rpc.testnet.chain.robinhood.com` | `wss://feed.testnet.chain.robinhood.com` | | |
| `optimism` | `https://mainnet.optimism.io` | | | |
| `base` | `https://mainnet.base.org` | | | |
| `unichain` | `https://mainnet.unichain.org` | | `wss://mainnet-flashblocks.unichain.org/ws` | `https://mainnet-sequencer.unichain.org` |
| `ink` | `https://rpc-gel.inkonchain.com` | | | |

Public endpoints make no uptime, latency or rate-limit promises. A production instance normally passes its own JSON-RPC endpoint with `-rpc`.

DEX deployments bound to each preset:

| Preset | Factories | Uniswap V4 |
|---|---|---|
| `arbitrum-one` | Uniswap V2, Uniswap V3, PancakeSwap V3, Camelot V3, Slipstream, Solidly V3 | yes |
| `robinhood` | Uniswap V2, Uniswap V3, Slipstream | yes |
| `optimism` | Uniswap V2, Uniswap V3, Slipstream, Velodrome V2, Velodrome V1, Solidly V3 | yes |
| `base` | Uniswap V2, Uniswap V3, Slipstream | yes |
| `unichain` | Uniswap V2, Uniswap V3 | yes |
| `ink` | | yes |
| `robinhood-testnet` | | |

Any other Nitro or OP Stack chain can be described with `-network-file`. A profile needs a nonzero parent chain ID that differs from its own ID, and a Nitro profile must state its ordering.

## Startup identity checks

- The RPC endpoint's `eth_chainId` must equal the profile's chain ID.
- The sequencer feed's `Arbitrum-Chain-Id` and server version headers must match when present. Profiles with `feed_headers_optional` (all Nitro presets) accept a handshake without them, because the relays do not always send them; blocks still come from the chain-checked RPC.
- Fast Feed frames carry no chain identity, so the endpoint itself must be trusted to serve the selected chain.
- `OP_EXECUTION_RPC_URL`, when set for execution paper, must report the same chain ID.

## Canonical and tentative state

Canonical pool state advances only from blocks and receipts the RPC endpoint has served and the follower has checked for hash, parent and receipt order. Feeds do one of two things:

- Wake the canonical follower early. The Nitro sequencer feed does this; it carries complete signed transactions but no receipts or logs, so the follower polls RPC for the announced block hash.
- Update a separate tentative branch. Fast Feed and Flashblocks receipts change a tentative pool view that search can read. They never write canonical state, and their prefixes are reconciled against canonical receipts as those arrive. A gap, contradiction or reconnect invalidates the tentative branch.

Identifiers synthesized for tentative prefixes are not block hashes.

## Ordering

| Ordering | Chains | Effect on bidding |
|---|---|---|
| `priority` | Arbitrum One, OP Stack chains | Transactions within a round or flashblock are sorted by priority fee, ties by arrival. The bid model prices the tip. |
| `arrival` | Robinhood | First come, first served; a tip does not move a transaction ahead. Rosetta signs the minimum tip and the bid model treats every rival on the route as arriving first. |

Arbitrum One sorts by priority fee in 125 ms rounds (a priority gas auction). There is no public mempool on either family, so a strategy can only back-run transactions it has seen ordered.

## Feeds

### Nitro sequencer feed

The sequencer broadcast is a WebSocket stream of JSON frames, one per L2 block, each with a `sequenceNumber`, the block's signed transactions and its `blockHash`. It is not JSON-RPC and accepts no subscriptions. The client resumes after a reconnect by requesting the next sequence number. With `-event-source sequencer`, each frame's block hash wakes the RPC follower.

Some relays require permessage-deflate (`feed_compression` in the profile; set for the Robinhood presets).

### Arbitrum Fast Feed

Fast Feed publishes each transaction as soon as the sequencer orders it, before the block closes. A frame holds the PGA round, a tentative block number and transaction index, the raw transaction and a partial receipt with status, gas and logs but no block hash. There is no replay. The client authenticates with `Authorization: Bearer <key>` from `ROSETTA_FAST_FEED_KEY`; one key admits a limited number of connections.

`-fast-feed` is Nitro only and requires the two index flags below. Whether a back-run sent after a Fast Feed frame lands in the same block is `UNVERIFIED` for any given setup; measure it before relying on it.

### Fast Feed index mapping

The public schema does not say where system transactions appear, so the mapping between a frame's `tx_index` and the canonical receipt index must be measured, never assumed. `cmd/fastprobe` records frames over one connection, reads the same blocks' canonical receipts, and matches them by transaction hash:

```bash
ROSETTA_FAST_FEED_KEY=... ROSETTA_RPC_URL=... \
  go run ./cmd/fastprobe -blocks 200 -out fast-frames.jsonl
```

| Flag | Default | Meaning |
|---|---|---|
| `-feed` | `wss://arb1-txfeed.arbitrum.io/` | Fast Feed URL. |
| `-blocks` | `200` | Complete blocks to observe. |
| `-timeout` | `10m` | Overall deadline. |
| `-out` | | Optional new JSONL file with every received frame. |

The report on stdout lists candidate `(fast_first_index, canonical_first_index)` pairs in `mappings`, ordered by the number of blocks that agree. Use the top pair for `-fast-first-index` and `-fast-canonical-first-index` only when `consistent_blocks` equals `blocks`, `unmatched_frames` (tentative positions that did not become final) is zero, and `omitted_receipts` (canonical receipts no frame carried) is zero. The report also gives frames and PGA rounds per block, the receive-time spread within a block, and `arrival_ms`, which includes the clock offset between the host and the sequencer. A reconnect aborts the probe. Measure again after every ArbOS upgrade.

The probe never signs or sends.

### OP Stack Flashblocks

Flashblocks (Subblocks on Unichain) stream several partial blocks per L2 block as JSON or Brotli-compressed JSON with `payload_id`, `index`, `base`, `diff.transactions` and `metadata.receipts`. Rosetta attaches at an index-zero frame whose parent matches the verified RPC anchor and then applies receipts in order to the tentative branch.

A stream without receipts is rejected unless `-flashblocks-receipts-rpc` is set, which fetches receipts with `eth_getTransactionReceipt` batches from a Flashblocks-aware RPC at roughly one call per transaction. Stream state roots and block hashes are placeholders and never become canonical identifiers.

Only the `unichain` preset ships a stream URL. For other OP Stack chains pass `-flashblocks` explicitly.

## Fees

### Nitro

A Nitro transaction's `gasUsed` already includes the gas charged for its L1 data (poster gas); the receipt's `gasUsedForL1` splits it out and is not added again. The paid fee is:

```text
gasUsed × effectiveGasPrice
```

Because L1 cost is paid in L2 gas, a transaction's gas limit must cover both execution and poster gas. The reactive executor reads `ArbGasInfo.getPricesInWei()` (precompile `0x…6c`) and adds poster gas to each intent's limit: twice the per-transaction plus per-byte charge at the uncompressed calldata bound, divided by the current base fee. The fee budget is the gas limit times the fee cap, and `max_transaction_fee` bounds it.

Nitro also accepts `eth_sendRawTransactionConditional`, whose storage-slot and timestamp conditions are checked at the transaction's execution position. A transaction whose conditions fail is dropped without charge. The reactive `hot_path` and `packed_conditions` options rely on this and are rejected on OP Stack.

### OP Stack

An OP Stack transaction pays three separate charges:

| Charge | Source |
|---|---|
| L2 execution gas | `gasUsed × effectiveGasPrice` |
| L1 data fee | `GasPriceOracle` predeploy (`0x420…000F`), priced from the compressed transaction size |
| Operator fee | `GasPriceOracle`, linear in gas used, on chains that enable it |

`exec.OPFees` budgets them separately: gas limit times fee cap, plus an upper bound for the L1 data fee and one for the operator fee, all quoted from the oracle at a pinned block. The reactive executor reserves twice the quoted L1 and operator fees because data fees can move between quote and inclusion. This reserve is a policy estimate; it does not cap what the chain may charge.

## Unverified items

Mark a fact `UNVERIFIED` until a measurement settles it. Open items in the code today:

- Robinhood testnet ordering is assumed to match mainnet.
- Same-block landing after a Fast Feed frame depends on network position and has to be measured per deployment.
- OP Stack block-building checks for conditional transactions are not measured, which is why conditional routes are Nitro-only.
