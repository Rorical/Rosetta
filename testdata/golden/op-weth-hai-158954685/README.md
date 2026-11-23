# op-weth-hai-158954685

OP Mainnet block 158954685. After transaction 6 (zero-based), two Uniswap V3 pools priced WETH and HAI differently enough for a WETH → HAI → WETH cycle; transaction 7 executed that cycle.

| File | Content |
|---|---|
| `snapshot.bin` | Both pools and their tick windows at the parent block, in Rosetta's pool snapshot JSON encoding |
| `blocks.jsonl` | Complete receipt logs of the block's first 8 transactions. A prefix, not a full block. |
| `expected.json` | Quotes before the trigger, after it and after the next transaction; the actual WETH gain, gas and fees of transaction 7; and Rosetta's simulated executor gas and L1 fee bound |
| `manifest.json` | Source, capture and verification details, compiler settings of the fork check, checksums |

The offline regressions (`internal/pipeline`, `internal/arb`) check that coarse ranking and amount optimization still find the cycle at the trigger state and that the quotes match the recorded values.

A historical fork check, recorded in the manifest, confirmed that the executor's balance change matched the pool-math quote to the unit and that its profit guard rejected the trade before the trigger and after the next transaction. At the historical gas price, Rosetta's simulated execution of this route would have had a negative net; `expected.json` records both fee bases.

A pool snapshot is not a full EVM world state; repeating the fork check needs a historical RPC. The case proves neither ordering position nor that Rosetta sent the transaction.
