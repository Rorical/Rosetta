# Agent and contributor conventions

Rules for anyone changing this repository, human or AI coding agent. [CONTRIBUTING.md](CONTRIBUTING.md) covers setup and the pull-request process; this file covers the invariants that review enforces.

## Project shape

- Rosetta is an L2 arbitrage engine. One process serves one chain, and every chain goes through the RPC backend in `internal/remote`. Do not add an in-process node or an L1 execution backend ([ADR 0001](docs/adr/0001-one-process-per-chain-rpc-backend.md)).
- Chain families are `nitro` (Arbitrum One, Robinhood Chain) and `op-stack` (OP Mainnet, Base, Unichain, Ink). Nothing else is supported.
- The code moves real funds when run live. Treat any change to `internal/exec`, `internal/planning`, signing, or `contracts/src` as security-sensitive and test it accordingly.

## Languages and tools

- Go 1.25 (see `go.mod`), module path `github.com/Rorical/Rosetta`. Production builds use `CGO_ENABLED=0`; the entry point is `make build`.
- Contracts: Solidity 0.8.26 with Foundry ([contracts/README.md](contracts/README.md)).
- Console frontend in `web/`: TypeScript and Vite, embedded into `rosetta-console` by `web/embed.go` (`make build-console`).
- Python only under `scripts/`, for offline bid-model training and paper-run maintenance. Never at runtime.
- Rust only for tests: `internal/testutil/opengine` is the native OP execution reference behind `make check-op`.
- Format Go with `gofmt` and Solidity with `forge fmt`. Comments, docs, UI text and commit messages are in English.
- Library code never panics; return errors with context. `context.Context` is the first parameter.
- Log with `log/slog` structured fields. Do not build large strings on the hot path.
- Runtime parameters come from CLI flags (`internal/config/flags.go`) and the config files they name. Environment variables carry secrets and keyed endpoints (for example `ROSETTA_FAST_FEED_KEY`, `ROSETTA_SIGNING_SECRET_ID`, `ROSETTA_TRACE_RPC_URL`, `ROSETTA_SUBMIT_RPC_URL`); the only non-secret exceptions are `ROSETTA_LOG_LEVEL`, `ROSETTA_WATCHDOG` and `ROSETTA_FEED_ARRIVALS_OUT`. Document every new flag or variable in [docs/configuration.md](docs/configuration.md).

## Checks

| Change touches | Run before committing |
|---|---|
| Any Go code | `make fmt-check`, `go vet ./...`, `go test -race -timeout=240s ./...` |
| OP confirmation or fee logic (`internal/remote`, `internal/exec`, `internal/planning`) | also `make check-op` (needs Rust and CGO) |
| `web/`, `internal/console`, `internal/control` | `make check-console` |
| `contracts/` | `make forge-test` |
| `internal/bidmodel`, `scripts/bid-learning/` | `make check-bid-learning` |

`make check` runs everything. CI runs the same targets.

## Layering

Imports follow this direction. A new import must not point the other way or create a cycle.

```text
cmd/rosetta          -> {bot, remote, telemetry}
cmd/rosetta-console  -> {console, web}
cmd/<offline tools>  -> shared packages; never remote
bot         -> {chain, config, domain}
chain       -> {config, domain}
remote      -> shared packages; RPC, feeds and signers are assembled only here
pipeline    -> {arb, backtest, dex, dex/<protocol>, domain, exec, graph, ingest, math, pool, simulate, state, telemetry}
planning    -> {arb, dex, dex/<protocol>, domain, exec, pool, telemetry}
discovery   -> {bootstrap, dex, dex/<protocol>, domain, graph, math, pool, rpc}
catalog     -> {bootstrap, dex/<protocol>, domain, exec, pool}
ingest      -> {dex/<protocol>, domain, feed, pool, rpc, state}
backtest    -> {arb, domain, graph, ingest, math, pool, state}
bootstrap   -> {dex/<protocol>, domain, pool, state}
exec        -> {domain, rpc, tokenrisk}
state       -> {dex/slipstream, dex/uniswapv3, domain, pool}
arb         -> {domain, graph, math, pool}
graph       -> {domain, pool}
simulate    -> {domain, pool}
dex         -> {dex/<protocol>, domain, pool}
dex/<protocol> -> {domain, math, pool} (V3 derivatives also use dex/uniswapv3)
pool        -> {domain, math}
feed        -> {domain}
rpc         -> {domain}
telemetry   -> {rpc}
control     -> {config, telemetry}
console     -> {control, domain, telemetry}
bidreport, paperreport      -> {domain, exec}
config, tokenrisk, poolrisk -> {domain}
domain, math, bidmodel, rundir -> no internal imports
```

- `internal/domain` holds shared types and invariants only. No I/O.
- `internal/pool` defines the adapter interface; protocol implementations live in `internal/dex/<protocol>`. The `internal/dex` root only registers protocols and shares decoding.
- `internal/feed` handles transport and framing. It does not read state or make trading decisions.
- `internal/ingest` applies receipt logs to pool state. It uses `rpc` types and does no network I/O.
- State transitions in `internal/state` are deterministic: no wall clock, no randomness, no dependence on map iteration order ([ADR 0004](docs/adr/0004-deterministic-state-transitions.md)).
- Only `internal/exec` signs and sends transactions. Signers come from AWS Secrets Manager, a version-pinned Google Secret Manager secret, or an explicitly enabled `keystore://` ([ADR 0021](docs/adr/0021-signing-only-in-exec.md)).
- `internal/console` holds no keys. It influences an instance only through control-socket gates, which act at plan delivery and in the send queue, never inside state transitions ([ADR 0022](docs/adr/0022-console-holds-no-keys.md)).
- `internal/bot` validates chain identity and wires the lifecycle. RPC, feed and storage logic do not belong there.
- Tools under `cmd/` other than `rosetta` never sign or send transactions.

## Domain invariants

- Chain IDs: Arbitrum One `42161`, Robinhood Chain `4663` (testnet `46630`, parent Sepolia), OP Mainnet `10`, Base `8453`, Unichain `130`, Ink `57073`. The parent chain ID must be nonzero and differ from the chain's own. Never reuse another family's execution or fee semantics by changing a chain ID ([ADR 0002](docs/adr/0002-chain-families-and-identity.md)).
- Ordering: Arbitrum One orders by priority fee, Robinhood Chain by arrival, OP Stack chains by priority fee.
- Canonical state advances only from whole blocks whose parent matches the canonical head. Blocks fetched from RPC are verified on arrival; blocks sealed from a complete feed stream enter on probation and are checked against RPC (every block at first, then on a sample), with unverified blocks rechecked after any mismatch. Fast Feed and Flashblocks prefixes update a separate speculative branch, never canonical state, and must not borrow an unrelated canonical snapshot to produce a ready plan. A synthetic prefix identifier is not a block hash ([ADR 0003](docs/adr/0003-canonical-state-and-speculative-branches.md)).
- Fast Feed first-index mappings (`-fast-first-index`, `-fast-canonical-first-index`) are measured with `cmd/fastprobe` before they are configured. Never hard-code them.
- Fees: Nitro poster gas is part of transaction gas. OP Stack L2 gas, L1 data fee and operator fee are budgeted separately (`exec.OPFees`) ([ADR 0011](docs/adr/0011-fee-budgets-per-chain-family.md)).
- A provider's trace or execution capability, or a same-block backrun bound, is unverified until measured. Say so in code comments and docs instead of assuming it.

## Safety

- Never commit private keys, mnemonics, keystores, `.env` files, or RPC and feed URLs that embed a key.
- Never commit hostnames, IP addresses, account identifiers, deployment records, run directories, or the addresses of anyone's live wallets and executors. `configs/` holds only public pool catalogs and policy files; reactive configs are committed only as `*.example.json` templates.
- Live reactive configs must set per-transaction and daily fee caps (`max_transaction_fee`, `max_daily_fee`), a principal cap on every start asset (`assets[].max_input`), and a `stop_file` ([ADR 0018](docs/adr/0018-mandatory-live-safety-limits.md)). Do not add a code path that sends without them.
- Pools and intermediate tokens are not allowlisted. Exposure is bounded by discovery scope, the transfer short-pay registry (`internal/tokenrisk`) and pool quarantine ([ADR 0017](docs/adr/0017-token-risk-and-pool-quarantine.md)).

## Tests

- Unit tests never touch the network or sign paid transactions. Use local fake servers or sockets for RPC and feeds, and `forge test --offline` for contracts.
- Prefer table-driven tests for pure functions and state transitions.
- Math, decoders and graph search need fixtures or vectors from real chain data. Small fixtures go in `testdata/fixtures/` or the package's `testdata/` (under 1 MB per file); replay regressions go in `testdata/golden/` (under 5 MB per case). See [docs/testing.md](docs/testing.md).
- Fix a bug by writing the failing test first. Run the golden replays after any strategy or state change.
- Large datasets, replays and run output (`state/`, `research/`, candidate JSONL, run roots) stay out of git.
- Do not add tests that mirror documentation changes.

## Documentation and decisions

- When architecture, the execution path or strategy changes, update the affected file in `docs/` and add or supersede an ADR in `docs/adr/` (next free number; mark the old record `Superseded by NNNN` instead of rewriting it).
- Flags, environment variables, config fields and operating procedures go in [docs/configuration.md](docs/configuration.md). Chain and endpoint facts go in [docs/chains.md](docs/chains.md).
- Mark unmeasured claims as unverified and name the measurement that would settle them. Do not commit live results, P&L, or deployment logs.

## Commits

- Conventional commits in English with the package or area as scope, for example `feat(dex)`, `fix(remote)`, `perf(arb)`, `config(arbitrum)`. Reference the ADR in the body when a commit implements one.
- Keep code changes and documentation-only changes in separate commits.
- Review `git diff --cached` for personal paths, hostnames, IPs and account data before every commit.
