# Commands

`cmd/rosetta` is the trading process; `internal/bot` wires it to the RPC backend. Every other command is an operator or offline tool. Only `rosetta` signs or sends transactions, and only with an explicit live reactive configuration.

| Command | Purpose | Network access |
|---|---|---|
| `rosetta` | Follow one L2, maintain pool state, search cycles, confirm plans, run execution paper or reactive execution. See [configuration.md](../docs/configuration.md). | RPC, feeds, submission endpoint |
| `rosetta-console` | Web console for running instances and their delivery gates. Holds no keys. | Control sockets, Prometheus |
| `fastprobe` | Measure the Arbitrum Fast Feed index mapping for `-fast-first-index` and `-fast-canonical-first-index`. See [chains.md](../docs/chains.md#fast-feed-index-mapping). | Fast Feed, RPC |
| `capturecheck` | Compare a historical arbitrage case with local path search and quotes at the pre-block state, optionally replaying the block's earlier logs. | RPC, with an `eth_call` disk cache |
| `routeaudit` | Diagnose route coverage from `-replay-out` captures: replay known routes, extract rival cycles from receipts (`-auto`), and build an omission funnel. | none |
| `paperaudit` | Summarize an execution paper journal, or with `-live` the paid settlements in a reactive journal. | none |
| `bidaudit` | Score recorded bid predictions against canonical outcomes: calibration, age and lane buckets, failure evidence, and net over all paid attempts. | none |
| `bidmodel` | Validate a GBDT bid artifact and print Go predictions for feature vectors. See [bid-learning.md](../docs/bid-learning.md). | none |
| `cyclebench` | Benchmark cycle search and V2 quoting on a synthetic universe. Measures timing and recall only. | none |

`make build` builds `rosetta`, `routeaudit`, `paperaudit`, `bidaudit` and `bidmodel` into `bin/`. `make build-console` builds `rosetta-console`. Build the others with `go build ./cmd/<name>`. Each command prints its flags with `-h`.

Examples:

```bash
./bin/routeaudit -capture replay.jsonl -search-config configs/optimism-search.json -auto -out audit.jsonl
./bin/paperaudit -input execution-paper.jsonl
./bin/paperaudit -live -input research/reactive.jsonl
./bin/bidaudit -input research/reactive.jsonl -since 2026-01-01T00:00:00Z -until 2026-01-02T00:00:00Z
go run ./cmd/capturecheck -case case.json -rpc "$L2_RPC_URL"
go run ./cmd/cyclebench -depth 12 -cycles 1000
```

`bidaudit` selects sends by submission time in `[since, until)` and still accepts outcomes that arrive after `until`. Its metrics describe the tips that were actually sent; they do not show what a different tip would have won.
