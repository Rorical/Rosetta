# Configuration

A `rosetta` process follows exactly one L2 chain through its JSON-RPC endpoint. Runtime parameters come only from command-line flags. Environment variables carry secrets, endpoints that embed an API key, and a few process-level switches.

Chain identities, ordering and fee semantics are in [chains.md](chains.md). Test entry points are in [testing.md](testing.md).

## Build

```bash
make build                      # bin/rosetta, bin/routeaudit, bin/paperaudit, bin/bidaudit, bin/bidmodel
make web-deps build-console     # bin/rosetta-console with the embedded web UI
```

Production builds use `CGO_ENABLED=0`. `make build` stamps the git revision into the binary; it is reported by `/status` on the control socket and by `rosetta_build_info`.

## Run an instance

Every run needs an RPC endpoint (`-rpc`, or the network preset's public RPC) and exactly one source of initial pool state: `-pool-config`, `-pool-catalog` or `-pool-snapshot`. At startup the process checks that the endpoint's `eth_chainId` matches the selected network.

Follow Arbitrum One with the sequencer feed waking the RPC follower, search for cycles and write candidates:

```bash
./bin/rosetta -network arbitrum-one \
  -rpc "$L2_RPC_URL" \
  -event-source sequencer \
  -pool-catalog configs/arbitrum-one-catalog.json \
  -search-config configs/arbitrum-one-search.json \
  -candidate-out candidates.jsonl \
  -pool-snapshot-out pools.json \
  -metrics-listen 127.0.0.1:9090
```

The `unichain` preset includes a Flashblocks stream, so `-event-source auto` (the default) selects it with no extra flags:

```bash
./bin/rosetta -network unichain \
  -rpc "$L2_RPC_URL" \
  -pool-catalog configs/unichain-catalog.json \
  -search-config configs/unichain-search.json \
  -candidate-out candidates.jsonl
```

A long-running paper instance on OP Mainnet that resumes from a snapshot and rotates its outputs into run directories:

```bash
./bin/rosetta -network optimism \
  -rpc "$L2_RPC_URL" \
  -pool-snapshot "$STATE/resume.json" \
  -search-config configs/optimism-search.json \
  -research-config configs/optimism-research-paper.json \
  -discovery-config configs/optimism-discovery.json \
  -run-root "$STATE" -control-dir "$STATE/control" \
  -metrics-listen 127.0.0.1:9090
```

Reactive execution, first as a preflight that validates the deployed executor and exits, then in shadow mode (no signing):

```bash
./bin/rosetta -network arbitrum-one -rpc "$L2_RPC_URL" -event-source sequencer \
  -pool-catalog configs/arbitrum-one-catalog.json \
  -search-config configs/arbitrum-one-search.json \
  -reactive-config reactive.json -reactive-preflight

./bin/rosetta -network arbitrum-one -rpc "$L2_RPC_URL" -event-source sequencer \
  -pool-catalog configs/arbitrum-one-catalog.json \
  -search-config configs/arbitrum-one-search.json \
  -reactive-config reactive.json \
  -token-risk-file state/token-risks.jsonl -pool-risk-file state/pool-risks.jsonl \
  -control-dir state/control -metrics-listen 127.0.0.1:9090
```

Live mode additionally needs `"mode": "live"` in the reactive config, a submission endpoint (`-submit-rpc` or `ROSETTA_SUBMIT_RPC_URL`) and a signing key reference in `ROSETTA_SIGNING_SECRET_ID`. See [reactive configuration](#reactive-configuration) and [signing](#signing).

SIGINT and SIGTERM stop the process gracefully: workers drain, then `-pool-snapshot-out` and `-metrics-out` are written. Logs are JSON records on stderr. With `-json`, each accepted canonical block is also written to stdout as one JSON object (`block` with number, hash, parent hash, timestamp and L1 block; `reset` is true when a reorg reseeded the pools).

## Environment variables

Secrets and keyed endpoints:

| Variable | Used when | Meaning |
|---|---|---|
| `ROSETTA_FAST_FEED_KEY` | `-fast-feed` is set | Arbitrum Fast Feed API key, sent as a bearer token. |
| `ROSETTA_SIGNING_SECRET_ID` | reactive `mode: live` | Signing key reference for the primary sender. See [signing](#signing). |
| `ROSETTA_KEYSTORE_PASSWORD_FILE` | a `keystore://` reference | File holding the keystore passphrase; trailing CR/LF is trimmed. Takes precedence over the next variable. |
| `ROSETTA_KEYSTORE_PASSWORD` | a `keystore://` reference | Keystore passphrase. |
| `ROSETTA_SUBMIT_RPC_URL` | reactive `mode: live` | Sequencer submission endpoint when neither `-submit-rpc` nor the network preset supplies one. |
| `ROSETTA_SUBMIT_FANOUT_RPC_URLS` | reactive `mode: live` | Comma-separated extra submission endpoints. Each signed transaction is also sent to these; unreachable entries are left out with a warning. |
| `ROSETTA_TRACE_RPC_URL` | reactive | Endpoint with `debug_traceTransaction`, used by `preflight_simulation` and to diagnose reverted sends. Falls back to `-rpc`. |
| `OP_EXECUTION_RPC_URL` | OP Stack, `-execution-paper-config` or reactive live | Separate endpoint for executor simulation (execution paper checks its chain ID) and, in live mode, for revert diagnosis. Takes precedence over `ROSETTA_TRACE_RPC_URL` on OP Stack. |
| `ROSETTA_HISTORY_LOGS_RPC_URL` | `-discovery-config` | Endpoint for wide historical `eth_getLogs` queries, for a primary RPC that caps log ranges. |
| `ROSETTA_DISCOVERY_STATE_RPC_URL` | `-discovery-config` | Endpoint for discovery state reads, for a primary RPC that serves state for only recent blocks. |
| `ROSETTA_CONSOLE_TOKEN` | `rosetta-console` | Operator login token, at least 24 bytes. |

Process switches:

| Variable | Default | Meaning |
|---|---|---|
| `ROSETTA_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; records below the level are dropped. |
| `ROSETTA_WATCHDOG` | `90s` | Longest a main-loop handler may run. On a stall the process writes every goroutine stack to stderr and exits with status 75 so a supervisor restarts it. `0` disables the watchdog. |
| `ROSETTA_FEED_ARRIVALS_OUT` | unset | With `-event-source sequencer`, records the local arrival time of every feed message to this file. |

Keep keys and keyed URLs out of command-line arguments, which process listings show to other users.

## Flags

`rosetta -h` prints the same list. Durations use Go syntax (`250ms`, `2s`). All `*-out` paths except `-pool-snapshot-out` must not exist yet; the process refuses to overwrite an earlier run's output.

### Network and inputs

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-network` | string | `arbitrum-one` | Built-in preset: `arbitrum-one`, `robinhood`, `robinhood-testnet`, `optimism`, `base`, `unichain` or `ink`. |
| `-network-file` | path | | Custom Nitro or OP Stack network profile (JSON, see [below](#custom-network-profile)). Cannot be combined with `-network`. |
| `-rpc` | URL | preset RPC | L2 JSON-RPC endpoint for canonical blocks, receipts, state reads and reorg ancestry. Required when the profile has none. |
| `-rpc-heads` | URL | | WebSocket endpoint whose `newHeads` subscription wakes the canonical follower on each block. Not used with Flashblocks. |
| `-event-source` | string | `auto` | `rpc` polls only. `sequencer` lets the Nitro sequencer feed wake the RPC follower. `flashblocks` follows an OP Stack Flashblocks stream. `auto` picks `flashblocks` when a stream URL is configured, otherwise `rpc`. An explicit `rpc` ignores a preset's stream. |
| `-feed` | URL | preset feed | Nitro sequencer feed for `-event-source sequencer`. |
| `-flashblocks` | URL | preset stream | Raw Flashblocks/Subblocks WebSocket (`ws` or `wss`). OP Stack only; requires the `flashblocks` event source. |
| `-flashblocks-receipts-rpc` | bool | `false` | Allow `eth_getTransactionReceipt` batches for a stream that omits receipts. Costs about one RPC call per transaction. |
| `-fast-feed` | URL | | Arbitrum Fast Feed WebSocket. Nitro only; reads `ROSETTA_FAST_FEED_KEY`; requires both index flags below. |
| `-fast-first-index` | int | `-1` | First transaction index carried by Fast Feed in each block. Measure it with `cmd/fastprobe` ([chains.md](chains.md#fast-feed-index-mapping)). |
| `-fast-canonical-first-index` | int | `-1` | Canonical receipt index that corresponds to the first Fast Feed transaction. |
| `-submit-rpc` | URL | preset submit URL | Sequencer endpoint (`http` or `https`) for signed transactions. Falls back to `ROSETTA_SUBMIT_RPC_URL`. |

### RPC pacing

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-rpc-poll` | duration | `2s` | Canonical head polling interval. |
| `-rpc-wait` | duration | `30s` | How long startup and recovery wait for a block the RPC reports as null. `0` fails at once. |
| `-rpc-min-interval` | duration | `0` | Minimum interval between calls of the same method; batch members count separately. `0` disables pacing. |
| `-rpc-max-calls` | uint | `0` | Per-run budget of external request units, retries and batch members included. `0` is unlimited. |
| `-rpc-search-refresh` | string | `canonical` | `canonical` loads each candidate's exact dependencies over RPC. `logs` updates from receipt logs only, for candidate paper runs; it cannot be combined with `-execution-config`. |

### Pool state and risk registries

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-pool-config` | path | | Explicit pool list, read at one canonical head. |
| `-pool-catalog` | path | | Token and factory catalog; pools are resolved from verified factories. Examples: `configs/*-catalog.json`. |
| `-pool-snapshot` | path | | Pool snapshot written by `-pool-snapshot-out`. Its block number and hash must match a canonical block. |
| `-pool-snapshot-out` | path | | Accepted pool state, written once after a fresh bootstrap and again at shutdown. The file is replaced atomically. |
| `-discovery-config` | path | | Background factory discovery that adds pools while running. Requires `-search-config`. Examples: `configs/*-discovery.json`. |
| `-token-risk-file` | path | | Persistent, chain-bound registry of tokens that short-paid a transfer. Search and execution both skip them. |
| `-pool-risk-file` | path | | Persistent, chain-bound quarantine of pools that caused repeated execution reverts. |

Exactly one of `-pool-config`, `-pool-catalog` and `-pool-snapshot` is required. With `-execution-paper-config` and no explicit risk files, the registries default to `<paper output>.token-risks.jsonl` and `<paper output>.pool-risks.jsonl`.

### Search and observation

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-search-config` | path | | Search limits and funding bounds; enables cycle search. See [search configuration](#search-configuration). |
| `-candidate-out` | path | | Unconfirmed candidates as JSON lines. Requires `-search-config`. |
| `-research-config` | path | | Observation limits and explicit per-asset cost budgets. Requires `-search-config`. Example: `configs/research.json`. |
| `-research-out` | path | | Candidate economics, survival samples and confirmation outcomes as JSON lines. Requires `-search-config`. |
| `-replay-out` | path | | Canonical pool snapshots and full receipt logs as JSON lines, for offline replay with `routeaudit`. Stops the run on queue overflow or I/O error rather than writing a gap. |

### Execution

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-execution-config` | path | | Executor, funding, fee and valuation policy; enables full executor confirmation of candidates. Requires `-search-config`. |
| `-plan-out` | path | | Confirmed ready plans as versioned JSON lines. Requires `-execution-config`. |
| `-execution-paper-config` | path | | Read-only executor simulation with a persistent sample journal. OP Stack only; requires `-search-config`; excludes `-execution-config`. Example: `configs/optimism-execution-paper.json`. |
| `-reactive-config` | path | | Reactive execution policy in `shadow` or `live` mode. Requires `-search-config`; excludes `-execution-config`. See [reactive configuration](#reactive-configuration). |
| `-reactive-preflight` | bool | `false` | Validate the deployed executor and the cached trading context, then exit without loading a key or signing. Requires `-reactive-config`. |

### Operations and outputs

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-control-dir` | path | | Private directory for the operator socket and the persistent delivery gate. One instance per directory. See [control socket](#control-socket). |
| `-run-root` | path | | Create run directories under `DIR/runs/` and rotate outputs in process. Requires `-control-dir` and `-search-config`; replaces `-candidate-out`, `-research-out`, `-replay-out`, `-pool-snapshot-out` and `-metrics-out`. See [run directories](#run-directories). |
| `-metrics-listen` | address | | TCP address serving `/metrics` (Prometheus text) and `/healthz`, for example `127.0.0.1:9090`. |
| `-metrics-out` | path | | Final pipeline metrics as JSON, written after workers stop. |
| `-profile-listen` | address | | Loopback-only address serving Go runtime profiles under `/debug/pprof/`. |
| `-blocks` | int | `0` | Canonical blocks to process before exiting. `0` runs until interrupted. |
| `-json` | bool | `false` | Write one JSON object per accepted canonical block to stdout. |

## Configuration files

All JSON files are decoded strictly: unknown fields and trailing data are errors. Token amounts are decimal strings in the token's base units, so they keep full uint256 precision. Addresses are hex strings; the zero address stands for the chain's native currency.

### Custom network profile

`-network-file` takes the same structure the presets use (`internal/config/network.go`):

```json
{
  "family": "op-stack",
  "name": "example-l2",
  "chain_id": 999999,
  "parent_chain_id": 1,
  "rpc": "http://127.0.0.1:8545",
  "feed": "",
  "flashblocks": "ws://127.0.0.1:8546/ws",
  "submit": "http://127.0.0.1:8547",
  "ordering": "priority",
  "block_time_ms": 2000,
  "dex": {
    "factories": {"uniswap_v3": "0x1111111111111111111111111111111111111111"},
    "v4_pool_manager": "0x2222222222222222222222222222222222222222",
    "v4_state_view": "0x3333333333333333333333333333333333333333"
  }
}
```

| Field | Meaning |
|---|---|
| `family` | `nitro` (default) or `op-stack`. Selects execution and fee semantics. |
| `name`, `chain_id` | Required. |
| `parent_chain_id` | Required, nonzero and different from `chain_id`. |
| `rpc`, `feed`, `flashblocks`, `submit` | Default endpoints; flags override them. `flashblocks` is OP Stack only. |
| `ordering` | `priority` or `arrival`. Required for Nitro; OP Stack defaults to `priority`. |
| `block_time_ms` | Nominal block interval, used to convert durations to block counts. Defaults to 250 (Nitro) or 2000 (OP Stack). |
| `feed_compression` | Offer permessage-deflate on the sequencer feed. |
| `feed_headers_optional` | Accept a feed handshake without the version and chain ID headers; they are still checked when present. |
| `dex.factories` | Factory addresses keyed by `uniswap_v2`, `uniswap_v3`, `pancake_v3`, `slipstream`, `camelot_v3`, `velodrome_v2`, `velodrome_v1` or `solidly_v3`. |
| `dex.v4_pool_manager`, `dex.v4_state_view` | Uniswap V4 deployment; set both or neither. |

### Pool list

`-pool-config` lists pools explicitly:

```json
{
  "pools": [
    {"address": "0x...", "kind": "v3", "token0": "0x...", "token1": "0x...", "fee": 500, "tick_spacing": 10},
    {"address": "0x...", "kind": "v2", "token0": "0x...", "token1": "0x...", "fee": 3000}
  ]
}
```

Fields are `address`, `kind`, `token0`, `token1`, `fee`, `tick_spacing`, and for V4 `pool_id` and `hooks`. V4 pools also need the network's PoolManager and StateView. Only hookless V4 pools are quoted.

### Search configuration

`-search-config` (`internal/pipeline/search_policy.go`; examples `configs/*-search.json`):

| Field | Meaning |
|---|---|
| `k` | Candidate cycles ranked per dirty pool. Positive. |
| `exact_k` | How many of the `k` ranked cycles get the full amount search. Default `k`. |
| `max_depth` | Longest cycle, 2 to 16 pools. |
| `max_cycles` | Cycle table budget, or `"auto"` to size it from the memory limit and `GOMAXPROCS`. |
| `explore_paths` | Background explorer path cache, 1024 to 262144. Default 8192. |
| `word_radius` | Tick bitmap words loaded on each side of the current tick for concentrated-liquidity pools, 1 to 64. Default 1. |
| `fast_coarse_cycles` | Routes the reactive fast lane ranks per dirty pool. `0` ranks all. |
| `fast_lane_hops` | Longest route the fast lane holds, 2 to `max_depth`. Default 3. |
| `fast_lane_quotes` | Routes per funding asset the fast lane quotes exactly, 0 to `k`. Default 2. |
| `funding[]` | Start assets: `token`, `min_input`, `seed_input`, `max_input` (requires `0 < min ≤ seed ≤ max`), optional `min_gross`. |

### Other policy files

These are documented by their example files and loaders:

| Flag | Example | Loader |
|---|---|---|
| `-pool-catalog` | `configs/*-catalog.json` | `internal/catalog/catalog.go` |
| `-discovery-config` | `configs/*-discovery.json` | `internal/discovery/config.go` |
| `-research-config` | `configs/research.json`, `configs/arbitrum-one-research.json`, `configs/optimism-research-paper.json` | `internal/pipeline/research_policy.go` |
| `-execution-config` | none | `internal/planning/policy.go` |
| `-execution-paper-config` | `configs/optimism-execution-paper.json` | `internal/remote/execution_paper_policy.go` |

`-execution-config` takes `sender`, `executor`, `code_hash`, `gas_limit`, `max_fee_per_gas_wei`, `tip_per_gas_wei`, `max_total_fee_wei`, `fee_multiplier_bps`, `slippage_bps`, `valid_for_seconds`, `max_candidates`, `timeout_ms`, `native_usd_feed`, `sequencer_feed`, `sequencer_grace_seconds`, and `assets[]` with `token`, `funding`, `lender`, `max_loan_fee`, `min_net_profit`, `usd_feed` and `max_price_age_seconds`. It confirms plans without signing them.

## Reactive configuration

`-reactive-config` controls the reactive executor: it admits search results, builds executor calls, prices the tip, and in live mode signs and sends them under hard spending limits. The loader is `internal/remote/reactive_policy.go`. The example below is `configs/reactive.example.json`; `configs/arbitrum-one-reactive.example.json` is a Nitro starting point. Every address and hash in them is a placeholder.

```json
{
  "mode": "shadow",
  "bid_tree_mode": "off",
  "replacement_min_gain_bps": 100,
  "cancel_gas_limit": 30000,
  "cancel_failure_gas": 0,
  "sender": "0x1111111111111111111111111111111111111111",
  "executor": "0x2222222222222222222222222222222222222222",
  "code_hash": "0x1111111111111111111111111111111111111111111111111111111111111111",
  "wrapped_native": "0x4200000000000000000000000000000000000006",
  "journal": "state/reactive-dispatch.json",
  "output": "research/reactive.jsonl",
  "stop_file": "state/STOP_REACTIVE",
  "gas_limit": 2000000,
  "route_gas_base": 250000,
  "route_gas_per_step": 250000,
  "deadline_seconds": 6,
  "max_fee_per_gas": "1000000",
  "max_priority_fee_per_gas": "200000",
  "target_priority_fee_per_gas": "100000",
  "max_transaction_fee": "10000000000000",
  "max_daily_fee": "100000000000000",
  "bid_share_bps": 2500,
  "quote_reserve_bps": 1000,
  "slippage_bps": 50,
  "lifetime_ms": 300,
  "context_max_age_ms": 5000,
  "max_calldata_bytes": 2048,
  "assets": [
    {"token": "0x4200000000000000000000000000000000000006", "max_input": "1000000000000000", "minimum_net_profit": "100000000000", "wallet": true},
    {"token": "0x0b2c639c533813f4aa9d7837caf62653d097ff85", "max_input": "1000000", "minimum_net_profit": "100", "wallet": false}
  ]
}
```

### Safety limits

These fields are required, and the dispatcher refuses to start without them:

| Field | Unit | Meaning |
|---|---|---|
| `max_transaction_fee` | wei | Most one transaction may cost: gas limit times fee cap, plus reserved L1 and operator fees on OP Stack. |
| `max_daily_fee` | wei | Fee budget per UTC day, including reverted and unresolved transactions. Must be at least `max_transaction_fee`; with `lanes` it is split evenly over all senders. |
| `assets[].max_input` | token base units | Largest principal a route may start with in that asset. |
| `stop_file` | path | While this file exists, nothing new is signed or sent. Already broadcast transactions are not revoked. |
| `journal` | path | Dispatch journal holding nonces and every signed variant. Do not delete or edit it while any outcome is unknown. |

`journal`, `output` and `stop_file` must be distinct paths.

### Identity and paths

| Field | Meaning |
|---|---|
| `mode` | `shadow` builds and records intents without signing. `live` signs and sends. |
| `sender` | Operator account. The signing key must derive this address. |
| `executor`, `code_hash` | Deployed executor and its runtime code hash; both are verified on chain. |
| `wrapped_native` | Wrapped native token of the chain. |
| `output` | Append-only JSONL of intents, sends and settlements. Rival transactions go to `<output>.competition.jsonl`. |
| `lanes[]` | Extra senders, each with `sender`, `secret_id` (same forms as `ROSETTA_SIGNING_SECRET_ID`) and its own `journal`. |

### Gas and fees

| Field | Unit | Meaning |
|---|---|---|
| `gas_limit` | gas | Upper gas limit per transaction, 21,000 to 32,000,000. |
| `route_gas_base`, `route_gas_per_step` | gas | Optional per-route limit: base plus per-step times route length, capped by `gas_limit`. Set both or neither. |
| `funding_gas_limits` | gas | Optional limit per funding mode: `wallet`, `v3_flash`, `v3_flash_swap`, `v4_flash`, `inventory`. |
| `max_fee_per_gas` | wei | Fee cap ceiling. |
| `max_priority_fee_per_gas` | wei | Tip ceiling. May be `0`. |
| `target_priority_fee_per_gas` | wei | Ceiling for the rule-based tip when no bid model applies. May be `0`. |
| `base_fee_headroom_bps` | bps | Fee cap over the current base fee, 10000 to 40000. Default 20000. |
| `bid_share_bps` | bps | Share of the surplus above `minimum_net_profit` offered as the rule-based tip, at most 10000. |
| `deadline_seconds` | s | On-chain deadline after build time, 2 to 30. Default 2. |

On Nitro, each transaction's gas limit also carries poster gas for its L1 cost. On OP Stack, L1 data and operator fees are reserved on top of the gas budget. See [chains.md](chains.md#fees).

### Admission and quoting

| Field | Unit | Meaning |
|---|---|---|
| `assets[].token` | address | Start asset. The zero address is native currency. |
| `assets[].minimum_net_profit` | base units | Least profit after all reserved fees. |
| `assets[].wallet` | bool | The route may be funded from the sender's balance and allowance. Otherwise only flash funding is used. |
| `assets[].inventory` | bool | The route may be funded from the executor's own balance of this ERC20. |
| `quote_reserve_bps` | bps | Quote error reserve against input plus quoted gross, deducted before costs. Below 10000. |
| `slippage_bps` | bps | Minimum-output slippage per leg, at most 1000. |
| `lifetime_ms` | ms | Off-chain window in which a built intent may be sent, 1 to 2000. |
| `context_max_age_ms` | ms | Oldest cached trading context (balances, fees, executor version) an intent may use, `lifetime_ms` to 60000. |
| `max_calldata_bytes` | bytes | Largest executor calldata, 256 to 16384. |
| `fast_lane` | bool | Quote two- and three-hop routes through each feed frame's changed pools on the main loop, in parallel with the full search. |
| `long_tail` | bool | Send only routes that stayed profitable past the next block. |
| `skip_taxed_pools` | bool | Reject routes through Slipstream pools with a swap hook or MEV tax module. |
| `preflight_simulation` | bool | Run each intent as `eth_call` against the executor at its quote block before sending; a revert drops it. |
| `unproven_route_cover_bps` | bps | Send a Fast Feed intent on a route preflight never proved only when its expected net covers this share of its fee budget. `0` drops such intents. |

### Executor features

Each of these needs an executor build that supports it; otherwise the transaction reverts on chain and its gas is paid.

| Field | Meaning |
|---|---|
| `price_precheck` | Give every leg a price bound so a route whose pools moved reverts before any swap (packed calldata version 3). |
| `price_precheck_cumulative` | Check the bound on the cumulative rate (packed version 4). Needs `price_precheck`. |
| `metric_omm` | Admit routes through MetricOmm pools. |
| `wrap_native` | Treat wrap and unwrap as a 1:1 leg so routes can cross native-currency V4 pools. |
| `hot_path` | Send hot routes to `executeHot` as conditional transactions. Nitro only; needs `hot_chain`. |
| `hot_chain` | Use the chained hot route form, the only one the bundled executor implements. Needs `hot_path`. |
| `packed_conditions` | Send other routes conditionally on their quiet pools. Nitro only. |

### Pending transactions

| Field | Unit | Meaning |
|---|---|---|
| `replacement_min_gain_bps` | bps | Least relative net improvement for a same-nonce replacement, at most 10000. |
| `cancel_gas_limit` | gas | Gas limit of the zero-value self-transfer that cancels a pending send. |
| `cancel_failure_gas` | gas | Estimated gas of a failed executor call, used to decide whether cancelling is cheaper. `0` disables automatic cancellation. |
| `native_refill` | wei | Optional `{"below": ..., "target": ...}`. When the sender's native balance falls below `below`, executor profit in wrapped native is swept and unwrapped up to `target`. `below` must exceed `max_transaction_fee`; the wrapped native token must be a start asset. |

### Bid model

| Field | Default | Meaning |
|---|---|---|
| `bid_model` | `shadow` | `off`, `shadow` (record the model's tip, send the rule-based tip) or `apply`. |
| `bid_landing_ms` | 400 | A rival on the route within this window after admission is modeled as landing first. |
| `bid_window_ms` | 700 | Rivals within this window are bids to beat. Greater than `bid_landing_ms`, at most 5000. |
| `bid_min_samples` | 50 | Closed windows needed before the model prices tips. |
| `bid_min_win_probability` | 0.2 | Least calibrated win probability a modeled tip needs to be sent. `0` disables the gate. |
| `tip_on_expected_gas` | `false` | Reserve the tip in the profit guard on expected gas instead of the gas limit. Needs a bid model. |
| `gas_prior_bps` | 6000 | Expected gas as a share of the limit until receipts settle, 1000 to 10000. Needs a bid model. |
| `bid_archive` | `false` | Append bid windows and outcomes to `<output>.bids-archive.jsonl`. Needs a bid model. |
| `bid_tree_mode` | `off` | Experimental GBDT bidding: `off`, `shadow` or `apply`. Needs `bid_tree_file` and `bid_model: apply`. See [bid-learning.md](bid-learning.md). |
| `bid_tree_file` | | Path of the active tree model. |

Under `arrival` ordering the build signs the minimum tip, because a tip does not move a transaction ahead.

## Signing

Only `internal/exec` signs. A key reference is read from `ROSETTA_SIGNING_SECRET_ID` for the primary sender and from `lanes[].secret_id` for extra lanes. The derived address must equal the configured sender, or startup fails.

| Reference | Backend |
|---|---|
| plain secret ID or ARN | AWS Secrets Manager, using the default AWS SDK credential chain. The secret is a hex private key in `SecretString`. |
| `gcp://projects/PROJECT_NUMBER/secrets/SECRET/versions/VERSION` | Google Secret Manager, using Application Default Credentials. The project must be its number and the version a pinned number; aliases such as `latest` are rejected. |
| `keystore://PATH` | Local go-ethereum V3 keystore. `PATH` is the key file or a directory holding exactly one `UTC--*` file. The passphrase comes from `ROSETTA_KEYSTORE_PASSWORD_FILE` or `ROSETTA_KEYSTORE_PASSWORD`. |

The key is loaded once into process memory and never written to disk. A keystore and its passphrase on the same host can be decrypted by anyone who reads both, so keep only limited funds on that account. With systemd, a credential file keeps the passphrase out of the unit environment:

```ini
LoadCredential=keystore-password:/etc/rosetta/keystore-password
Environment=ROSETTA_KEYSTORE_PASSWORD_FILE=/run/credentials/rosetta.service/keystore-password
```

Recommended sequence for a new executor:

1. Build and test the contract (`make forge-test`). `scripts/prepare-executor.py` prepares unsigned deployment and operator calls; it reads no key and sends nothing.
2. Deploy and verify the runtime code hash, owner and operator. The executor deploys paused.
3. Run with `-reactive-preflight`. It validates code, configuration, balances and cache inputs, then exits without loading a key.
4. Run `mode: shadow` and review the intents in `output`.
5. Switch to `mode: live` with a submission endpoint and a key reference. Create `stop_file` to stop new sends at any time.

## Run directories

With `-run-root DIR`, the process creates `DIR/runs/<UTC yyyymmddThhmmss>-<pid>/` (with a `-2`, `-3` suffix on a collision) after the pool bootstrap succeeds, and atomically writes that path to `DIR/current-run`. Each run holds:

| File | Content |
|---|---|
| `search.json`, `research.json` | Copies of the loaded configuration. |
| `candidates.jsonl` | Candidates. |
| `observations.jsonl` | Research output. |
| `replay.jsonl` | Replay capture; starts with a pool snapshot so it replays on its own. |
| `final-pools.json` | Pool snapshot at the end of the run. |
| `final-metrics.json` | Counters since process start. |

`POST /rotate` on the control socket, or SIGHUP, closes the current run and opens the next without a restart. Every record lands in exactly one run. Reactive output, the competition file and the execution paper journal are appended across runs and never rotate.

`scripts/paper/maintain.py` maintains a run root offline:

```bash
python3 scripts/paper/maintain.py rotate  "$STATE"   # request a rotation, then close finished runs
python3 scripts/paper/maintain.py finish  "$STATE"   # after the process exits: promote final-pools.json to resume.json
python3 scripts/paper/maintain.py archive "$STATE"   # compress closed runs and prune old replays
```

`resume.json` is the snapshot for the next `-pool-snapshot`. The script makes no RPC calls and never touches a run that is still being written.

## Control socket

`-control-dir DIR` opens `DIR/ops.sock`, an HTTP API over a Unix socket, and keeps `DIR/gate.json` and a lock that admits one instance. The directory must be a real directory owned by the running user with no group or other access, because socket access is the only authorization. The full socket path must fit in 103 bytes.

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness. |
| `GET /metrics` | Prometheus text, as on `-metrics-listen`. |
| `GET /status` | Run ID, network, chain ID, event source, version, start time and the output files the console tails. |
| `GET /snapshot` | Current metrics as JSON. |
| `GET /gate` | Delivery gate state. |
| `PUT /gate` | `{"paused": true, "actor": "...", "reason": "..."}`; reason up to 200 bytes. |
| `POST /rotate` | Start the next run. Answers 409 without `-run-root`. |

```bash
curl --unix-socket "$STATE/control/ops.sock" http://localhost/status
curl --unix-socket "$STATE/control/ops.sock" -X PUT http://localhost/gate \
  -d '{"paused":true,"actor":"operator","reason":"maintenance"}'
```

A paused gate holds ready plans at delivery and blocks live reactive submits and replacements at the last check before sending. Cancellations still go out. Search, confirmation and research keep running. The gate survives restarts; an unreadable `gate.json` stops startup.

## Console

`rosetta-console` is a separate process that holds no keys and makes no chain calls. It reads each instance's control socket, imports its candidate and reactive output files into SQLite, and serves a web UI.

```bash
ROSETTA_CONSOLE_TOKEN="$(openssl rand -base64 32)" ./bin/rosetta-console \
  -instance arbitrum-one="$STATE/control" \
  -data-dir "$STATE/console" \
  -prometheus http://127.0.0.1:9091
```

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8788` | Listen address; must be loopback. |
| `-instance` | | `name=control-dir`, repeatable. Names use lowercase letters, digits and dashes. Required. |
| `-data-dir` | | Directory for `console.db`. Required. |
| `-prometheus` | | Prometheus base URL for charts and alerts. Without it the charts stay empty. |
| `-public-host` | | Host name an authenticating gateway serves the console under, repeatable. |

The console speaks plain HTTP. Reach it through an SSH tunnel or a gateway that terminates TLS. Every gate change is written to an audit log. The console serves its own `/metrics` (instance reachability and import lag) without a session.

## Metrics and alerts

`-metrics-listen` and the control socket serve the same registry:

| Family | Labels | Content |
|---|---|---|
| `rosetta_pipeline_events_total` | `source`, `event` | Counters: feed messages, RPC calls and errors, search, confirmation, reactive funnel. |
| `rosetta_pipeline_state` | `source`, `gauge` | Gauges: head block and timestamp, pools, feed connection and backlog, sync lag, graph size, delivery gate, dispatcher state, free disk. |
| `rosetta_pipeline_duration_seconds` | `source`, `stage` | Latency histograms per stage. |
| `rosetta_plan_estimate_total` | `source`, `item` | Simulated plan totals in gwei. These are estimates, not realized profit. |
| `rosetta_rpc_method_calls_total`, `rosetta_rpc_requests_total` | | RPC usage. |
| `rosetta_build_info`, `rosetta_start_time_seconds`, `rosetta_process_*` | | Build identity and process resources. |

`source` is `standard` for the canonical path or `fast` for tentative feed input.

[deploy/prometheus](../deploy/prometheus) holds a scrape configuration and alert rules. Each scrape target's `rosetta_instance` label must equal the console's `-instance` name. Because the example scrapes the bot on port 9090, run Prometheus itself on another port and pass that URL to `-prometheus`.
