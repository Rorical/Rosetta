# 24. Offline tests against recorded chain data and a native OP reference

## Status

Accepted

## Context

Most of the engine's failure modes are numeric: a decoder that misreads one event field, a tick crossing rounded the wrong way, a fee formula that drifts by one byte of calldata. Synthetic test values rarely hit these cases, and a wrong result looks plausible.

Tests that call live RPC endpoints or send paid transactions are slow, flaky, cost money and need credentials, which also rules them out for outside contributors. The repository has no CI service, so the local `make check` is the gate.

Confirming OP Stack plans relies on the provider's tracing. Comparing our confirmation against the same provider proves nothing about the provider.

## Decision

- Unit tests make no network calls and sign no paid transactions. RPC servers, feeds and streams are local fakes over HTTP, WebSocket or sockets. Contract tests run with `forge test --offline`.
- Math, decoders and graph search are checked against fixtures and reference vectors captured from real chains. Small shared samples go in `testdata/fixtures/` (under 1 MB per file), package-specific samples in the package's `testdata/`, and curated replay cases in `testdata/golden/` (under 5 MB per case, with a manifest and checksums). Larger datasets stay out of git; only their manifests are committed.
- A fixture pins one historical state: block, protocol version, funding and fee basis. Matching a final price alone is not enough.
- Pure functions and state transitions use table-driven tests. A bug fix starts with a failing test, and strategy or state changes rerun the golden replays.
- `make check-op` runs the compiled executor in a pinned native OP execution engine (`internal/testutil/opengine`, Rust, test-only) and compares output, gas, fees, nonce and state isolation with the Go confirmation path across the supported OP hardforks. It needs Rust and CGO, so it is a separate target required for changes to OP confirmation or fee logic.
- Bid learning has its own target (`make check-bid-learning`) for Go and Python inference parity.
- Claims about live behaviour that tests cannot cover (provider trace support, sequencer semantics, latency, opportunity lifetime) are marked `UNVERIFIED` with the measurement that would settle them.

## Consequences

- `make check` runs anywhere with Go, Node and Foundry, without accounts or keys.
- Capturing a fixture is part of adding a protocol or fixing a quote bug, which slows those changes and makes regressions in them rare.
- The OP reference engine is the only Rust in the tree and does not ship. There is no Nitro equivalent, so Nitro confirmation and fee evidence rely on unit tests with recorded responses and on Foundry fork tests run by hand.
- Passing golden cases shows the code reproduces history; it says nothing about the profit available on chain today.
