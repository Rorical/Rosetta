# Contracts

`ArbitrageExecutor` executes one closed swap route atomically: it either ends with the starting asset back plus at least `minProfit`, or it reverts. The off-chain runtime chooses routes, sizes and fees; the contract enforces access, the route's integrity and the profit floor.

## Build and test

Requires Foundry and Solidity 0.8.26. The first `forge build` installs the compiler; after that, tests run offline with no third-party Solidity libraries.

```bash
cd contracts
forge build --sizes
forge fmt --check
forge test --offline
```

`make forge-test` runs the last two from the repository root. The build targets the Cancun EVM (transient storage, EIP-1153) with `via_ir` and 8,000 optimizer runs (`foundry.toml`). Foundry's `out/`, `cache/`, `lib/` and `broadcast/` directories are ignored by git.

Fork tests replay against historical chain state and are skipped unless their variables are set:

| Variables | Tests |
|---|---|
| `ROSETTA_ARB_FORK_URL`, `ROSETTA_ARB_FORK_BLOCK` | `ExecutorArbitrumGasFork`, `ExecutorMetricFork`, `ExecutorWrapFork` |
| `ROSETTA_OP_FORK_URL`, `ROSETTA_OP_FORK_BLOCK` | `ExecutorPriceCheckFork` |

`ExecutorGas.t.sol` and the other `*Gas*` tests pin gas regression ceilings on synthetic pools; run `forge test --offline --match-contract ExecutorGasTest -vvvv` to see the measured values.

## Layout

| File | Content |
|---|---|
| `src/ArbitrageExecutor.sol` | Entry points `execute` and `executePacked`, funding modes, closing profit check |
| `src/HotV3Executor.sol` | `executeHot`, the compact route for conditional transactions |
| `src/FlashSwapExecutor.sol` | V3 flash-swap funding |
| `src/SwapExecutor.sol`, `src/V4SwapExecutor.sol` | Per-protocol swap legs and callbacks |
| `src/PackedPlan.sol` | Packed calldata decoder |
| `src/PriceCheck.sol` | Optional price prechecks before funding |
| `src/ExecutorAdmin.sol` | Owner, operators, pause, sweep |
| `src/ExecutionState.sol`, `src/TokenActions.sol`, `src/ExecutorTypes.sol` | Transient state, token transfers, shared types |
| `test/` | Offline unit, encoding, gas and fork tests with mock pools |
| `script/` | Empty; deployment calls are prepared by `scripts/prepare-executor.py` |

## Administration

The constructor takes the owner and up to two `executeHot` senders, fixes `chainId` to `block.chainid`, and leaves the contract paused. The owner starts as an operator.

| Function | Effect |
|---|---|
| `setOperator(address, bool)` | Allow or revoke an operator. |
| `setPaused(bool)` | Pause or resume `execute` and `executePacked`. |
| `sweep(token, amount)` | Withdraw accumulated profit to the owner. |

Every owner change increments `configVersion`, so off-chain caches know when to verify the contract again. Owner calls are refused while a route is executing. There is no pool or token registry: each route carries its own pool parameters.

`scripts/prepare-executor.py` turns a Foundry artifact and a setup file (`owner`, `operator`, `chain_id`, `owner_nonce`, and up to two `hot_senders` for `executeHot`) into unsigned deployment and activation transactions. It reads no key and sends nothing.

## Entry points

| Function | Caller | Input |
|---|---|---|
| `execute(Plan)` | operator | ABI-encoded plan |
| `executePacked(bytes)` | operator | Packed plan, versions 2 to 4 |
| `executeHot(bytes)` | a sender fixed at deployment | Compact V3-family route, sent only as a conditional transaction |

A plan names a funding mode, a start asset and amount, up to 16 steps, `minProfit` and a deadline. Each step gives the pool kind, target, output token, fee, tick spacing, minimum output and an optional price limit; the input token is the previous step's output.

### Funding modes

| Mode | Value | Principal |
|---|---:|---|
| `Wallet` | 0 | Pulled from the operator (ERC20 allowance, or `msg.value` for native) and returned at the end. |
| `V3Flash` | 1 | Borrowed from a separate Uniswap or PancakeSwap V3 pool with `flash`; the fee is capped by `maxLoanFee`, and the lender may not appear in the route. |
| `V3FlashSwap` | 2 | The first V3-family swap delivers its output first; the rest of the route runs inside its callback and pays the first pool at the end. ERC20 start asset only. |
| `V4Flash` | 3 | A closed route on one Uniswap V4 PoolManager settles only its net delta; no principal moves. |
| `Inventory` | 4 | The executor's own balance of an ERC20 is the principal. |

Profit stays in the executor until the owner sweeps it. The operator pays gas in every mode.

### Supported legs

| Kind | Value | Protocol |
|---|---:|---|
| `V2` | 1 | Uniswap V2 pairs |
| `V3` | 2 | Uniswap V3 and PancakeSwap V3 |
| `V4` | 3 | Uniswap V4 pools without hooks; consecutive legs on one PoolManager share one `unlock` and settle net |
| `CAMELOT_V3` | 4 | Camelot V3 (Algebra) |
| `SLIPSTREAM` | 5 | Aerodrome/Velodrome Slipstream |
| `VELODROME_V2` | 6 | Velodrome V2 stable and volatile pools |
| `SOLIDLY_V3` | 7 | Solidly V3 |
| `METRIC_OMM` | 8 | MetricOmm oracle-priced pools |
| `WRAP` | 9 | The wrapped native token itself: `deposit` or `withdraw` at 1:1 |

Native currency (the zero address) appears only in V4 and `WRAP` legs.

## Safety checks

- Callbacks are authenticated against the expected pool and payload and accepted once. There is no arbitrary call, `delegatecall` or router approval.
- Each leg checks its minimum output and that it consumed its full input. Partial fills revert.
- Intermediate token balances must be unchanged at the end of the route, and profit is measured on the asset's actual balance. A fee-on-transfer token or a short-paying pool therefore reverts the transaction instead of draining inventory.
- A wrong or hostile target can make the transaction revert but cannot take principal; the loss is bounded by gas.
- Per-transaction principal limits, daily fee budgets, token risk and pool quarantine are enforced off-chain by `internal/exec`.

### Price prechecks

Packed versions 3 and 4 let each leg carry a price reference read before any funding or swap:

- Version 3 bounds each pool's price one-sidedly in the leg's direction. A pool that moved past its bound reverts with `PoolMoved(step)`; an unreadable pool reverts with `UnreadablePool(step)`.
- Version 4 carries each leg's quoted price and a minimum cumulative rate, so several small adverse moves fail together.

A route that lost the race then reverts after a few storage reads instead of after every swap. Version 2 performs no precheck.

### Hot route

`executeHot` runs a V3-family flash-swap route in which each pool pays the next directly and intermediate tokens never reach the executor. It omits checks that the conditional transaction already enforces (pause state through slot 0, quiet pool slots, expiry through `timestampMax`) and keeps a one-sided price bound on the trigger pool, the full-input check and the closing profit check. A sent route can set a claim bit that increments `claimEpoch`, so when several variants of one opportunity race on different senders, the first success invalidates the others' conditions.

A plain, unconditional transaction to `executeHot` skips those conditions; only its fixed senders can call it, and the closing profit check still applies.

## Encoding

`internal/exec` builds both the ABI plan and the packed formats. Test vectors are checked by Go, Foundry and Solidity's ABI decoder. The packed format and the hot route layout are documented in `src/PackedPlan.sol` and `src/HotV3Executor.sol`.

## Not verified here

Offline tests use mock pools. Real token behavior, real flash fees, tick crossing on live pools and the receipt cost of each route need fork tests or chain receipts on the target network.
