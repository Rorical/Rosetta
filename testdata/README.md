# testdata

Shared test data. Layout, size limits and the golden case format are described in [docs/testing.md](../docs/testing.md#fixtures-and-golden-replays).

| Directory | Content | Limit |
|---|---|---|
| `fixtures/` | Small shared samples for unit and component tests: recorded feed frames, RPC responses, on-chain reference vectors | 1 MB per file |
| `golden/` | Curated historical replay cases, each with a manifest and checksums | 5 MB per case |

Package-specific fixtures live next to their package, for example `internal/dex/slipstream/testdata/` (dynamic fee and oracle vectors), `internal/tokenrisk/testdata/` (a transfer-shortfall trace) and `internal/arb/testdata/` (ranking and prefix vectors from real blocks).

Large datasets stay out of git. Keep only a manifest with source, capture time and checksums in the repository. When a data schema changes, migrate the golden cases with it.

A fixture reproduces one historical state. It pins the block, protocol version, funding and fee basis; matching a final price alone is not enough. Passing a golden case says nothing about the profit available on chain today.
