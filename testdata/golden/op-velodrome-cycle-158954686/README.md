# op-velodrome-cycle-158954686

OP Mainnet pool state and the canonical receipt prefix through a historical arbitrage transaction that crossed Velodrome pools. All state reads are pinned to the parent block. Replay applies pools at whole-transaction boundaries.

`expected.json` holds the route, the input and the actual gross gain in WETH, measured from the pools' own transfers independently of Rosetta's quote, plus the quoted gross before the trigger and after the arbitrage and the execution fees observed in the transaction trace.

The case checks dirty-path ranking, local amount optimization, shared route segments and cycle extraction from receipts (`internal/backtest`, `internal/pipeline`, `internal/arb`).

It does not prove inclusion, gas profitability or executor support for every pool kind.
