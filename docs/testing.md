# Testing

All checks run locally through the Makefile; the repository has no CI. Run the checks for the area you changed before committing, and `make check` before a release.

## Rules

- Unit tests make no network calls and never sign a paid transaction. RPC endpoints, feeds and the OP engine are replaced by local fake servers, sockets or subprocesses.
- Contract tests run with `forge test --offline`. Fork tests are skipped unless their environment variables are set.
- Pure functions and state transitions use table-driven tests.
- Math, decoders and graph search are checked against fixtures or reference vectors taken from real chain data.
- A bug fix starts with a test that fails without the fix.
- After a change to strategy or state handling, rerun the golden replays (below).
- Documentation changes get no tests that mirror the implementation.

## Which check to run

| Change | Run |
|---|---|
| Any Go code | `make fmt-check`, `go vet ./...`, `go test -race -timeout=240s ./...` |
| OP RPC confirmation or fee logic (`internal/remote`, `internal/exec`, `internal/planning`) | also `make check-op` |
| `web/`, `internal/console`, `internal/control` | `make check-console` |
| `contracts/` | `make forge-test` |
| `internal/bidmodel`, `scripts/bid-learning/` | `make check-bid-learning` |
| `scripts/paper/` | `python3 -m unittest discover -s scripts/paper -p 'test_*.py'` |

`make check` runs the format check, the build, `go vet`, the race tests, the console checks and the Foundry tests. It does not include `check-op` or `check-bid-learning`.

Prerequisites per target:

| Target | Needs |
|---|---|
| Go checks | Go 1.25 |
| `check-console` | Node.js and npm; run `make web-deps` once |
| `forge-test` | Foundry with Solidity 0.8.26 (the first `forge build` installs the compiler) |
| `check-op` | Rust (edition 2024), a C toolchain for CGO, and Foundry |
| `check-bid-learning` | Python 3 with `scripts/bid-learning/requirements.txt`; set `BID_PYTHON` to that interpreter |

## Test layers

| Area | Packages | What the tests pin down |
|---|---|---|
| Protocol math and state | `internal/math`, `internal/dex/...`, `internal/state` | Rounding, tick boundaries, atomic updates, undo, snapshot upgrades, dynamic fees and oracles |
| Search | `internal/arb`, `internal/graph` | Recall of real routes, amount optimization, coarse ranking, dirty dependencies, parallelism and cancellation |
| Sync and confirmation | `internal/remote`, `internal/rpc`, `internal/feed`, `internal/ingest` | Prefix integrity, RPC hash checks, sealing, gap recovery, reorgs, transient null headers |
| Bidding | `internal/planning`, `internal/remote`, `internal/bidmodel` | Opportunity windows, calibration, tip caps, replacement floors, tip-dependent pool fees |
| Execution | `internal/exec`, `internal/planning` | Fee admission, funding modes, nonce journal, unknown sends, replacement, cancellation, settlement |
| Risk | `internal/tokenrisk`, `internal/poolrisk` | Transfer shortfall from real traces, persistent exclusions, pool quarantine |
| Contracts | `contracts/test` | Funding modes, callback authentication, minimum output and profit, packed calldata, price prechecks, inventory |
| Operations | `internal/control`, `internal/console`, `web`, `scripts/paper` | Delivery gate, run rotation, console import, snapshot maintenance |

Test names and assertions define the exact coverage. Passing tests do not establish endpoint behavior, live latency or profitability.

## OP native engine check

`make check-op` compares Rosetta's OP Stack confirmation and fee accounting against a native reference executor:

1. Formats, builds and tests `internal/testutil/opengine`, a Rust binary built on `op-revm` that executes OP Stack transactions over a JSON line protocol.
2. Runs `go test -tags contracts,opnative -race` with `CGO_ENABLED=1` on `internal/remote`, `internal/exec` and `internal/planning`. These tests compile the executor with `forge build --offline` and start the engine as a subprocess.

`ROSETTA_OP_TEST_ENGINE` points the tests at an engine binary elsewhere; the default is `internal/testutil/opengine/target/debug/rosetta-op-test-engine`. Rust is a test-only dependency and never enters the production build.

## Contract tests

```bash
make forge-test        # forge fmt --check && forge test --offline, in contracts/
```

Fork tests run against historical chain state only when their variables are set:

| Variables | Tests |
|---|---|
| `ROSETTA_ARB_FORK_URL`, `ROSETTA_ARB_FORK_BLOCK` | `ExecutorArbitrumGasFork`, `ExecutorMetricFork`, `ExecutorWrapFork` |
| `ROSETTA_OP_FORK_URL`, `ROSETTA_OP_FORK_BLOCK` | `ExecutorPriceCheckFork` |

The URL is an archive-capable JSON-RPC endpoint. Fork results are measurements at one block, not regression bounds.

## Fixtures and golden replays

| Location | Content | Size limit |
|---|---|---|
| `testdata/fixtures/` | Shared small samples: feed frames, RPC responses, reference vectors | 1 MB per file |
| `internal/<package>/testdata/` | Package-local fixtures, for example Slipstream fee and oracle vectors or token shortfall traces | 1 MB per file |
| `testdata/golden/<case>/` | Curated historical replay cases | 5 MB per case |

A golden case holds:

| File | Content |
|---|---|
| `manifest.json` | Schema version, chain ID, block range, data source, capture and verification dates, SHA-256 of every data file |
| `snapshot.json` or `snapshot.bin` | Pool state at the parent block, in Rosetta's pool snapshot JSON encoding |
| `block.json` or `blocks.jsonl` | Canonical receipts of the target block up to and including the transaction of interest. This is a prefix, not a complete block. |
| `expected.json` | Expected route, amounts and fees |
| `README.md` | What the case checks and what it does not prove |

A case pins the pre-state, the complete pool keys, the target transaction position, raw asset units and the fee basis. Without all of these it cannot support a same-state profit comparison.

The golden cases are read by tests in `internal/backtest`, `internal/pipeline`, `internal/arb` and `cmd/routeaudit`:

```bash
go test -race ./internal/backtest ./internal/pipeline ./internal/arb ./cmd/routeaudit
```

Large datasets, replay captures and run output (`state/`, `research/`, candidate JSONL, run roots) stay out of git. The repository keeps only manifests and checksums for them, with no credentials or personal configuration.

## Offline analysis tools

`capturecheck`, `routeaudit`, `paperaudit`, `bidaudit` and `cyclebench` analyze recorded data; see [cmd/README.md](../cmd/README.md). They are diagnostics, not tests. `capturecheck` reads historical state over RPC and is never part of the unit suite.
