# 7. Atomic execution in one executor transaction

## Status

Accepted

## Context

A cyclic arbitrage takes an asset through several pools and must end with more of it than it started with. If any leg fails or returns less than quoted, the route as a whole loses money.

On the target L2s there is no bundle service. Arbitrum's sequencer does not implement `eth_sendBundle`, and OP Stack sequencers accept single transactions. Nothing outside the transaction can guarantee that several transactions land together, in order, or not at all.

## Decision

- Each route executes as one transaction to the project's executor contract (`contracts/src/ArbitrageExecutor.sol`). Funding, every swap, any loan repayment and the profit check happen inside that call, and any failure reverts all of it.
- The contract has one explicit call branch per protocol kind and no arbitrary `call` or `delegatecall`.
- Every leg carries a minimum output. The route ends with a check that the asset balance covers principal, loan fee and a non-zero `minProfit`; otherwise it reverts.
- The plan carries a deadline. An expired plan reverts before any transfer.
- The owner is immutable and set at deployment. It sets operators, pauses and sweeps. Operators send routes. The contract starts paused. Every owner change increments `configVersion`, so off-chain caches know when to re-check.
- The contract stores `block.chainid` at deployment and refuses calls on another chain.
- Callback context and the execution lock use EIP-1153 transient storage, so nothing persists between calls. The contract keeps no daily limits; spending limits live in the off-chain dispatcher (ADR 0018).
- Profit stays in the contract as inventory until the owner sweeps it. Wallet-funded principal is returned to the operator in the same call. Inventory left from earlier routes cannot subsidize a later one: intermediate tokens must end where they started, and profit is measured against the balance at entry.
- Routes run one per transaction. Multi-candidate transactions that try several routes and keep the first success are not implemented: they add contract size and make every failed attempt pay for all candidates.

## Consequences

- The worst outcome of a bad quote, a stale state or a rival moving first is a reverted transaction that costs gas. ADRs 0012 and 0013 reduce that cost.
- The contract is the main risk surface for funds and must stay under the 24 KB code limit, which caps how many protocols and entry points it can hold.
- Changing the owner, the fixed hot-path senders or the bytecode requires a new deployment and updating the executor address and code hash in the reactive config.
- A transaction that lands after a rival's still pays gas for the call up to the point it reverts.
