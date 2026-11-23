# op-slipstream-cycle-158960654

OP Mainnet pool state and the canonical receipt prefix through a historical arbitrage transaction that crossed Slipstream pools. All state reads are pinned to the parent block. Replay applies pools at whole-transaction boundaries.

`expected.json` holds the route, the input and the actual gross gain in WETH, measured from the pools' own transfers independently of Rosetta's quote. It also holds the quoted gross before the trigger and after the arbitrage, and the execution fees observed in the transaction trace.

The case checks dirty-path ranking, local amount optimization and cycle extraction from receipts (`internal/backtest`, `internal/pipeline`, `cmd/routeaudit`). Quote equality with the actual gain uses the observed fees; Slipstream fees depend on time, so the parent-block fee model is recorded separately and can differ inside the block.

It does not prove inclusion, gas profitability or executor support for every pool kind.
