# Execution

Execution turns a quoted cycle into one atomic transaction against the executor contract, decides what it may cost, sends it, and books the result. This document covers the contract's safety model, funding modes, fee budgeting per chain family, the two kinds of executable output, conditional transactions, the nonce journal, limits and bidding.

Code: `contracts/src` (executor), `internal/planning` (funding, valuation, fee-aware construction, tip selection), `internal/exec` (calldata, fee models, plans and intents, signing, journal, submission, settlement) and `internal/remote` (the reactive runtime that drives them).

## Outputs

A quoted route can become one of three things:

| Output | Type | Evidence before sending | Sent by Rosetta |
|---|---|---|---|
| Ready plan | `exec.ReadyPlan` (`internal/exec/plan.go`) | The full executor call run at a pinned canonical block hash or a verified Flashblocks prefix, with balances, fees and versions checked | No; written for an external consumer through `-plan-out` |
| Reactive intent | `exec.ReactiveIntent` (`internal/exec/reactive_intent.go`) | Local exact quote, cached account and fee context, contract guards | Yes, in live mode |
| Execution paper | `internal/remote/execution_paper*.go` | The executor call simulated at a chosen state, off the send path | No |

The two executable types are never interchanged. A reactive intent carries no EVM evidence and is never labeled as confirmed. It exists because a full RPC simulation per candidate takes longer than the auction it competes in; the contract's own checks bound what can go wrong instead.

## Executor contract

`contracts/src/ArbitrageExecutor.sol` executes one closed route in one transaction. Every step either succeeds or the whole transaction reverts. The contract is built with Solidity 0.8.26 and Foundry (see `contracts/README.md`).

### Entry points

| Function | Input | Notes |
|---|---|---|
| `execute(Plan)` | ABI-encoded `Plan` | Reference entry point, used for ABI cross-checks |
| `executePacked(bytes)` | Packed plan, versions 2 to 4 | Default; shorter calldata, which matters where calldata pays an L1 data fee |
| `executeHot(bytes)` | Compact chained route | Conditional-transaction-only route for V3-family pools, V2 pairs and MetricOmm pools (below) |

The packed format (`contracts/src/PackedPlan.sol`) is big-endian and fixed-width. The header holds version, funding mode, step count, asset, amount, `minProfit` and deadline; standalone V3 flash loans append the lender, its token side and `maxLoanFee`. Each step holds kind, target, `tokenOut`, fee, tick spacing, `minAmountOut` and flags, optionally followed by a sqrt price limit and a price field. `tokenIn` is never encoded: it is the previous step's `tokenOut`, or the asset for the first step. Truncated input, trailing bytes, unknown funding modes and step counts outside 1 to 16 are rejected. `internal/exec/packed.go` encodes the same layout.

### Supported pools

Step kinds match `domain.PoolKind`: Uniswap V2 pairs, V3 (Uniswap and PancakeSwap, distinguished by their callback), Camelot V3 (Algebra callback), Slipstream, Velodrome V2, Solidly V3, MetricOmm, Uniswap V4 and native wrap/unwrap. V4 legs always use a `PoolKey` with zero hooks and an empty `hookData`; consecutive V4 legs on the same PoolManager run in one `unlock` and settle only net deltas.

### Funding modes

| Mode | Principal | Repayment |
|---|---|---|
| `Wallet` | Pulled from the operator with `transferFrom` (needs an allowance), or native currency sent as `msg.value` | Principal returned to the operator at the end |
| `Inventory` | The executor's own balance of the asset | None; the closing balance check keeps the principal whole |
| `V3Flash` | Borrowed from a V3 pool's `flash` | `amount + fee` repaid inside the flash callback; the fee comes from the callback and must not exceed `maxLoanFee` |
| `V3FlashSwap` | None; the first V3-family swap delivers its output before its callback collects the input | The rest of the route runs inside that callback, which then pays the first pool |
| `V4Flash` | None; every leg is V4 on one PoolManager | Flash accounting inside one `unlock`; only net profit is taken |

`V3Flash` refuses a lender that also appears in the route, because the lender is locked during its flash. Balancer and Aave flash loans are not implemented.

`internal/planning/funding.go` lists the modes available for a route from cached state (wallet balance and allowance, executor inventory, whether the first pool can flash swap, whether the route is all V4 on one manager, and V3 lenders holding at least twice the principal), and `ChooseFunding` picks the one with the highest net profit after loan fee and network cost. Ties prefer inventory and callback funding, then the wallet, then the lender with the lowest address.

### Safety model

The contract keeps no pool registry, token allowlist or per-token cap. Pool parameters travel in calldata. Rosetta discovers pools permissionlessly, so a registry would either lag discovery or need an owner transaction for every new pool. Instead, the following checks bound a wrong or hostile target to a reverted transaction:

- Access. `owner` and `chainId` are immutable. Only registered operators may call `execute` and `executePacked`; the owner is an operator from construction. The contract starts paused. `paused` and a `configVersion` counter share storage slot 0, and every pause or operator change increments `configVersion`, so off-chain caches and conditions can detect any reconfiguration.
- Plan checks. Every call checks `block.chainid`, the deadline, a step count of 1 to 16, a nonzero `minProfit`, and that the route starts and ends at the asset.
- No generic calls. There is no arbitrary `call` or `delegatecall`; each protocol kind has one fixed call path.
- Per-leg minimums. Every leg checks its output against `minAmountOut` (`SwapExecutor.checkOutput`); the last leg also checks principal plus `minProfit`. The amounts a pool reports must match the requested input exactly and show a real output.
- Callback authentication. Before calling a pool, the executor records the expected target, protocol kind and a hash of the callback payload in EIP-1153 transient storage. A callback must come from that target with that kind and payload hash, and consumes the record before paying, so it runs once. If a call returns with the record still pending, the transaction reverts. Flash loans move through a small status machine in the same way.
- Inventory. Before the route, the executor records its balance of every intermediate token; at the end each must be exactly unchanged. Fee-on-transfer tokens, partial fills and short payments surface here, at the next payment, or at the profit check, and old inventory cannot be used to fake profit.
- Final profit. The asset balance at the end must exceed the starting balance (plus the principal in wallet mode) by at least `minProfit`. For flash loans the fee has already been repaid at that point, so profit is net of it.
- Native currency appears only on V4 legs or on a wrap leg; the contract accepts ether only from the PoolManager or wrapped-token contract it is currently settling with.

Profit stays in the executor. The owner withdraws it with `sweep`, which is only allowed while no execution is in progress.

Caps on principal, daily fees and funding assets live off-chain in the dispatcher (below); the contract has no daily accounting.

### Price prechecks

When a rival lands first on one of the route's pools, the transaction would otherwise run every swap and fail only at the profit check, paying for all of them. Packed versions 3 and 4 add an early check (`contracts/src/PriceCheck.sol`) that reads each bounded pool before funding and the first swap:

| Version | Check | Revert |
|---|---|---|
| 3 | A one-sided bound per leg: the V3-family or V4 sqrt price (from `slot0`, Algebra `globalState`, or the V4 pool's slot0 word through `extsload`), or the V2/Velodrome reserve ratio as Q96 | `PoolMoved(step)`, or `UnreadablePool(step)` |
| 4 | The product of every bounded leg's rate change since the quote must stay above `minRate`, so several small adverse moves fail together; favorable moves offset adverse ones up to a cap | Same |

Favorable moves always pass. `planning.SetPriceBounds` (`internal/planning/price_bounds.go`) gives each leg the whole slack: with `required = amount + minProfit + maxLoanFee` and `output` the quoted route output, a leg's bound is the price at which its rate has fallen by `required/output`. A single pool can absorb the route's entire margin before the check fires, which is exactly the move the profit check would reject anyway if the other pools stayed put. Stable Velodrome, MetricOmm and wrap legs carry no bound. The checks are enabled by `price_precheck` and `price_precheck_cumulative` in the reactive configuration.

### Hot routes

`executeHot` (`contracts/src/HotV3Executor.sol`) is a cheaper path for routes of V3-family pools, V2 pairs and MetricOmm pools. Pools pay each other directly through nested callbacks, intermediate tokens never reach the executor, and only the asset's final profit is checked. It skips the pause flag, deadline and per-leg minimums; its callers are two sender addresses fixed at deployment.

Those checks move into the transaction's conditions instead. The bot sends a hot route only as a conditional transaction that holds the executor's slot 0 (unpaused, current `configVersion`), the storage of the route's quiet pools and a `timestampMax`. A pause, a reconfiguration, a moved quiet pool or an expired route is then rejected at the sequencer without charge. The pool that just moved cannot be held by a condition and carries a price bound instead. A successful hot route can also raise `claimEpoch`; variants of one opportunity raced on several lanes all hold the current epoch as a condition, so the first to land voids the rest.

`hot_path` and `packed_conditions` are accepted only on Nitro chains (`internal/remote/reactive_policy.go`), whose sequencer documents this condition behavior.

## Fees

Each chain family charges differently, and the fee model must match the family. `exec.FeeModel` (`internal/exec/fees.go`) validates fee evidence and computes a conservative budget.

| Family | What the sender pays | Budget |
|---|---|---|
| Nitro | One gas amount that already includes the L1 (poster) component | `gasLimit × feeCap`. Reactive sending adds poster gas priced from ArbGasInfo `getPricesInWei`, with headroom. Confirmation uses `NitroRPCFees` (`fee_accounting = nitro-rpc-total`), which validates the total native charge without inventing an L1/L2 split |
| OP Stack | L2 execution gas, an L1 data fee and an operator fee, charged separately | `OPFees` (`internal/exec/op_fees.go`): `gasLimit × feeCap` plus an L1 fee upper bound plus an operator fee upper bound |

On OP Stack the L1 fee depends on the signed transaction's compressed size, which is not known while planning. `OPL1SizeBound` uses an upper bound that covers signature bytes and worst-case compression, and the GasPriceOracle prices it at the planning state. The reactive context cache reads `getL1FeeUpperBound` and `getOperatorFee` from the GasPriceOracle at two sizes, interpolates, and applies headroom. Its cache key follows the L1 attributes deposit, so it refreshes when fee parameters change. For confirmation, `exec.FeeBudgetEstimator` (`internal/exec/snapshot.go`) prices state-dependent fees at the confirmation snapshot; OP planning requires it.

On Nitro, the receipt's `gasUsed` already includes poster gas; adding `gasUsedForL1` again would double-count it.

### Profit floor

The fee budget is converted into the starting asset and encoded into the contract's `minProfit`, together with the asset's minimum net profit. The contract cannot see the final network fee, so the floor is what keeps a successful execution from losing money on fees. Conversion uses state-bound prices from the same snapshot (`planning.Valuer`), rounded against the trade.

Reactive admission also subtracts a quote reserve: `ceil((input + net) × quote_reserve_bps / 10000)`. It is taken from the route's final output including principal, because quote error scales with the amount swapped.

### Tip-dependent pool fees

Some Slipstream deployments charge a fee that grows with the transaction's priority fee. Search quotes at a zero tip and shares those quotes across candidates. For the few routes that reach planning, `planning.QuoteTransactionBound` recomputes every leg at the chosen tip cap, assuming a zero base fee and no account discount (the worst case). The signed intent records each such pool's fee, and validation checks that it was priced at the signed tip.

## Confirmed plans

With `-execution-config`, the runtime confirms candidates against a pinned state and builds ready plans (`planning.Build` in `internal/planning/build.go`):

- Nitro: the executor call runs through `debug_traceCall` at an EIP-1898 pinned block hash, once with a JavaScript tracer that captures the call result, gas and the sender's charge and nonce, and once with `prestateTracer` in diff mode to rebuild the post-state. The two must agree, and the same balance checks as on OP Stack apply.
- OP Stack: the sender balance is overridden to cover the full upfront fee (L2 gas, L1 bound and operator bound) and the call runs under `callTracer`; `prestateTracer` rebuilds the post-state, and the runtime checks that the executor's balance grew by the reported profit, that the sender paid only fees and got its principal back, that the owner's balance did not move, and the nonce, accepting no balance change it cannot explain (`fee_accounting = op-rpc-reserved`).
- Flashblocks prefixes: a candidate from the speculative branch can be confirmed at the exact prefix it was found on. The runtime first asks the node for the prefix state with `rosetta_getFlashblocksStateV1`; if the method does not exist (`-32601`), it replays the prefix's raw transactions on the canonical parent with `debug_traceBlock` and a mux of `callTracer` and `prestateTracer`. Standard OP nodes do not provide the first method.

Arbitrum Fast Feed candidates have no prefix confirmation path and never become ready plans.

A ready plan binds the full route, the call, fee and conversion evidence, the state, graph and policy identities, and a validity window. Its ID is a hash of the whole content. `ReadyPlan.ValidateForDispatch` checks chain ID, current state version, graph and policy, the window, rebuilds the calldata and requires it to be identical, revalidates fees and recomputes economics. Plans pass the control gate (see [architecture.md](architecture.md#operations)) before they are written out.

## Reactive intents

The reactive runtime (`internal/remote/reactive_*.go`) turns optimized quotes into intents without waiting for a simulation. Its configuration is a JSON file; `configs/arbitrum-one-reactive.example.json` is a template.

1. Candidates come from the search worker and from the fast lane (see [search.md](search.md#fast-lane)).
2. For the best few by gross profit, planning chooses funding, probes smaller principals, prices fees and the tip, reprices tip-dependent pool fees, and checks the profit floor (`planning.BuildReactive`).
3. The intent is sealed with a short lifetime (at most 2 s) and a budget that must cover `gasLimit × feeCap` plus any extra fees.
4. A queue keeps at most one job per lane. A new job replaces a queued or running one only if it is better; an arbitrage always beats a cancellation.
5. Before signing, the dispatcher rechecks the intent, pool versions, token risk, cache freshness, the stop file and the limits.

Account balances, allowances, executor state and fees come from a background cache, never from an RPC call on the send path. The cache verifies the executor's code hash, that it is unpaused, that every sending lane is an operator, and its chain ID, and verifies again whenever `configVersion` changes (`internal/remote/reactive_cache.go`).

`mode: "shadow"` builds and records unsigned intents without sending. `mode: "live"` signs and sends.

### Submission

Intents are sent with `eth_sendRawTransaction` to the submit endpoint (`ROSETTA_SUBMIT_RPC_URL`), which is configured separately from the read endpoint. The submit client is isolated from the read request pacing, so queued reads never delay a send, and it keeps its connection warm. `ROSETTA_SUBMIT_FANOUT_RPC_URLS` optionally sends the same signed bytes to more endpoints; the first acknowledgement wins, and identical bytes can be included at most once.

An acknowledgement only means the endpoint accepted the transaction. It proves nothing about execution.

### Conditional transactions

On Nitro, `eth_sendRawTransactionConditional` attaches `knownAccounts` (expected storage values) and timestamp bounds to a transaction. The sequencer checks them on admission and again at the transaction's execution position; a mismatch drops the transaction with error `-32003`, charges no gas and consumes no nonce. Conditions are not an ordering guarantee.

Rosetta uses them for hot routes and, with `packed_conditions`, for packed routes whose pools have been quiet. A pool qualifies as quiet after 12 unchanged blocks (`quietBlocks`), because Nitro also checks conditions against a state at least 2 s old. Its expected storage words (price and liquidity for V3-family pools, the packed reserves word for V2, the pool's state slots under the PoolManager for V4) are read once in the background, derived from logs afterwards, and spot-checked against the chain periodically. A quiet pool held by a condition needs no price bound.

A `-32003` rejection restores the nonce and budget reserved before the send and rewrites the journal. Conditional transactions are never replaced; a lost one is rebroadcast with its original conditions. Conditional sends do not fan out.

## Nonces, replacement and settlement

`internal/exec` owns every nonce. No other package reads or writes one.

### Journal

Each sending lane has a dispatch journal (`internal/exec/dispatch_store.go`): every signed variant, its nonce, its fee reservation and its settlement. The journal is a JSON file written through a temporary file, fsync, rename and a directory fsync, under a file lock. The write runs concurrently with the broadcast; if it fails, the dispatcher refuses every later send until restart. On startup the dispatcher refuses to resume if the chain's nonce moved in a way the journal cannot explain.

A send that times out or disconnects has an unknown outcome. Its nonce and full budget stay reserved until a feed sighting, a canonical receipt or a nonce read resolves it, across restarts.

### Lanes

`lanes` adds senders beyond the primary one. Each lane has its own key, journal and nonce sequence, and must be an operator of the executor. One search hands each lane at most one intent, and a route whose pool another lane is already sending is dropped. Wallet-funded routes use the primary lane only, and `max_daily_fee` is split evenly across lanes. With extra lanes, a better intent takes a free lane instead of replacing a pending transaction.

### Release and settlement

A nonce is consumed as soon as any signed variant of it appears in the sequencer feed or in a canonical block. At that point the dispatcher stops replacing it and the next send uses the next nonce. Up to eight released nonces may wait for their paid fees (`maxIncluded` in `internal/exec/lane_release.go`); when that many are pending, the lane stays closed.

A settlement records success or revert, the executor's profit after loan fee and the feed position when the transaction is first seen. Paid fees come later from the canonical receipt. If the canonical block disagrees with the feed, the canonical result wins and the settlement is rebooked. A canonical revert is traced once to record any token that short-paid (`internal/tokenrisk`).

The settlement watcher (`internal/exec/settlement_watch.go`) runs off the send path:

- A transaction seen neither in the feed nor in a block within 30 s triggers a nonce read. If one of its variants consumed the nonce, its receipt is booked. If an unrecorded transaction consumed it, the dispatcher halts.
- A transaction missing from the canonical block at the height it was seen, or still not included, is rebroadcast once with its exact signed bytes. If it is still missing afterwards, the dispatcher halts.
- A conditional transaction whose `timestampMax` has passed by 15 s without inclusion releases its nonce and budget.

Rebroadcasts obey the stop file and halts like any send. Rosetta never assumes that a transaction it has seen was withdrawn.

### Replacement and cancellation

An unresolved nonce can take up to three same-nonce replacements (`internal/exec/replacement.go`). Each must raise both fee cap and tip by at least 10%, pass funding, fee, profit and version checks again, and improve net profit by at least `replacement_min_gain_bps`. All signed variants are kept, and the budget reserves the most expensive one.

When rivals move the pools of a pending route, the runtime requotes it. If every signed variant is known to be unprofitable and a cancellation costs less than the expected failure cost (`cancel_failure_gas`), it can send a zero-value self-transfer at the same nonce (`internal/exec/cancellation.go`). Any unknown quote or still-profitable variant suppresses cancellation, and `cancel_failure_gas: 0` disables it. A cancellation is a replacement attempt; it cannot undo a transaction that already executed.

## Limits

Live reactive configuration must set:

| Setting | Enforced in | Effect |
|---|---|---|
| `max_transaction_fee` | `exec.Dispatcher`, `planning` | No intent whose fee budget exceeds it |
| `max_daily_fee` | `exec.Dispatcher` | Reserved and paid fees per day, split across lanes |
| `assets[].max_input` | `exec.Dispatcher` | Principal cap per starting asset |
| `stop_file` | `exec.Dispatcher` | While the file exists, nothing is signed or rebroadcast |

Funding assets are listed explicitly. Pools and intermediate tokens on a route are not allowlisted; they are limited by discovery scope, by short-pay evidence in `internal/tokenrisk` and by pool quarantine in `internal/poolrisk`.

If a paid fee exceeds its reservation, the dispatcher halts. Persistent failures halt too: an unexplained nonce, a failed journal write or a canonical receipt that does not verify. A halted dispatcher sends nothing new.

## Bidding

Priority-fee chains order transactions by tip, so each intent needs a tip that wins often enough without giving the profit away. On arrival-ordered chains such as Robinhood the tip does not affect ordering.

The rule-based tip spends a share of the expected surplus: `surplus × bid_share_bps / 10000`, divided over the expected gas and capped by `target_priority_fee_per_gas` and `max_priority_fee_per_gas`.

The probability model (`planning.ChooseTip` in `internal/planning/bid_model.go`) learns from the chain itself. The runtime watches successful multi-pool transactions from other senders, groups them into opportunity windows, and estimates how often rivals appear and how much they bid relative to the opportunity's value (`internal/remote/bid_*.go`). Our own outcomes calibrate the resulting win probability. For each candidate tip `t` it maximizes

```text
E(t) = P(t) · (surplus − t·gas) − (1 − P(t)) · failureCost(t)
```

where the failure cost uses the gas a losing transaction is expected to burn. The tip is bounded by the configured caps, and a candidate whose calibrated win probability is below `bid_min_win_probability` is not sent. `bid_model` is `shadow` (record the decision, use the rule-based tip), `apply` or `off`.

An optional gradient-boosted tree model (`internal/bidmodel`, `bid_tree_mode`) can replace the statistical estimates with predictions of win probability, revert gas and delay. Training runs offline in `scripts/bid-learning`; see [bid-learning.md](bid-learning.md).

The observer sees successful rival transactions only; it has no record of rivals' failed attempts or of their costs. Win probabilities are estimates conditioned on our own valuation of the opportunity.

## Accounting

```text
quotedGross    = route output − principal
admissionNet   = quotedGross − quoteReserve − loanFee − networkBudget
executorProfit = actual route profit after the actual loan fee
paidNet        = executorProfit − convert(receipt network fee)
```

`Settlement.Profit` is already net of the loan fee. Native fees come from receipts; asset-denominated net uses the conversion frozen at admission and is labeled as such. While fees are pending or unknown, `Net` stays empty. Shadow intents, paper simulations and live settlements are kept in separate records, and only live settlements count as realized results (`cmd/paperaudit -live`).

## Keys

Signing happens only in `internal/exec`. `exec.LoadSecretSigner` (`internal/exec/signer.go`) loads the key from AWS Secrets Manager, a pinned Google Secret Manager version (`gcp://projects/N/secrets/S/versions/V`), or, when explicitly configured, a `keystore://` file whose password comes from `ROSETTA_KEYSTORE_PASSWORD_FILE` or `ROSETTA_KEYSTORE_PASSWORD`. The derived address must equal the configured sender. Keys stay in process memory and never reach logs or disk. Remote KMS or HSM signing is not implemented.

The contract separates the immutable owner from operators. A hot wallet should hold only the funds its limits allow.

## Execution paper

Execution paper (OP Stack only) samples optimized quotes and runs the real executor calldata at the discovery state and at a bounded number of later prefixes on a separate worker. Results (success, revert, unsupported, unavailable, censored) go to their own journal. Simulation never enters signing, submission or ready-plan output, and search never waits for it. `internal/paperreport` and `cmd/paperaudit` aggregate the journals offline.
