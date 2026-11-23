# Contributing to Rosetta

Bug reports, fixes, new DEX adapters, and new chains are welcome. For anything larger than a bug fix, open an issue first so the design can be agreed before you write the code.

Security issues go through the private channel in [SECURITY.md](SECURITY.md), never a public issue.

## Development setup

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.25 (see `go.mod`) | everything |
| Foundry | recent stable | `contracts/` |
| Node.js | 22 | the console in `web/` |
| Rust | stable | `make check-op` (OP execution cross-check, uses CGO) |
| Python | 3.12 | `scripts/bid-learning` |

```sh
git clone <repository-url> rosetta && cd rosetta
make build            # bin/rosetta and the offline audit tools
go test ./...         # unit tests, no network access
```

Production binaries build with `CGO_ENABLED=0`. Only the OP cross-check needs CGO.

## Checks before a pull request

Run the base set for any Go change, plus the row for each area you touched. CI runs all of them.

| Area | Command |
|---|---|
| Any Go code | `make fmt-check`, `go vet ./...`, `go test -race -timeout=240s ./...` |
| OP confirmation or fee logic (`internal/remote`, `internal/exec`, `internal/planning`) | `make check-op` |
| Console (`web/`, `internal/console`, `internal/control`) | `make check-console` |
| Contracts (`contracts/`) | `make forge-test` |
| Bid model (`internal/bidmodel`, `scripts/bid-learning/`) | `make check-bid-learning` |

`make check` runs the full set.

## Code conventions

- Format Go with `gofmt` and Solidity with `forge fmt`. Comments and commit messages are in English and explain why, not what.
- Library code returns errors with context and does not `panic`. `context.Context` is the first parameter.
- Log with `log/slog` structured fields. Do not build large strings on the hot path.
- Runtime parameters come only from CLI flags (`internal/config/flags.go`). Environment variables are reserved for secrets and endpoints that embed a key. Document every new flag or variable in [docs/configuration.md](docs/configuration.md).
- Respect the package layering in [docs/architecture.md](docs/architecture.md). New imports must not point upward or create a cycle.
- State transitions in `internal/state` are deterministic: no wall clock, no randomness, no dependence on map iteration order.
- Only `internal/exec` signs and sends transactions.

## Tests

- Unit tests never touch the network or sign paid transactions. Use local fake servers or sockets for RPC and feeds, and `forge test --offline` for contracts.
- Prefer table-driven tests for pure functions and state transitions.
- Math, decoders, and graph search need fixtures or vectors taken from real chain data. Small fixtures go in `testdata/fixtures/` or the package's `testdata/` (under 1 MB per file). Replay regressions go in `testdata/golden/` (under 5 MB per case).
- Fix a bug by writing the failing test first.
- Large datasets, replays, and run output stay out of git.

## Adding a DEX

1. Implement the protocol in `internal/dex/<protocol>`. The adapter interface lives in `internal/pool`; `internal/dex` only registers protocols and shares decoding.
2. Port the swap math with integer arithmetic and check it against on-chain quotes in a fixture.
3. Decode the events that move pool state so `internal/ingest` can apply receipts.
4. If the executor needs a new leg type, add it in `contracts/src` with Foundry tests, including a callback-authentication test.

## Adding a chain

Chains belong to a family (`nitro` or `op-stack`) that fixes fee and ordering semantics. A new chain reuses its family's semantics; it must not borrow another family's behavior by changing a chain ID. Add the network preset (chain ID, parent chain, public RPC and feed, ordering, block time) in `internal/config/network.go` or `internal/config/op_network.go`, record its public facts in [docs/chains.md](docs/chains.md), and measure any feed index mapping with `cmd/fastprobe` instead of hard-coding it.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) with the package or area as scope, for example `feat(dex): add Algebra pools` or `fix(remote): reconnect feed after gap`.
- Keep pull requests focused. Separate refactors from behavior changes.
- A change to architecture, the execution path, or strategy needs an ADR in [docs/adr/](docs/adr/).
- Check your diff for keys, keyed URLs, hostnames, IP addresses, and personal paths before pushing.

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
