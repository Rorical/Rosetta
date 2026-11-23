# 18. Mandatory live safety limits

## Status

Accepted

## Context

A live instance signs transactions with a hot key and spends real funds on every send, including the ones that revert. Bugs in quoting, a bad fee read, a token trap or a runaway loop can each send many losing transactions within minutes. The executor contract keeps no spending limits (ADR 0007), so the limits must live off-chain, and they must hold even when the process restarts or the operator is not watching.

## Decision

The reactive policy file is rejected at startup unless it sets every limit below. There are no defaults for them.

- `max_transaction_fee`: the most one transaction may cost, in native wei. Every signed variant reserves its maximum possible fee (gas limit times fee cap, plus OP data and operator fees) until it settles.
- `max_daily_fee`: the most all transactions may cost per UTC day, counted from settled fees plus open reservations. It must be at least the per-transaction cap. With several send lanes it is split evenly so the sum never exceeds it, and each share must still cover one transaction.
- `max_fee_per_gas` and `max_priority_fee_per_gas`: hard caps on what any bid can sign.
- `assets[]`: the only start assets, each with `max_input` (principal cap) and `minimum_net_profit`.
- `stop_file`: a path whose existence stops all sending, cancellations included. It is checked before each send.
- `executor` and `code_hash`: the deployed contract and its expected runtime code, verified against the chain before trading.
- `journal`, `output` and `stop_file` must be distinct paths.

Other bounds are validated alongside: the gas limit (21,000 to 32 million), intent lifetime (at most 2 s), context age (at most 60 s), slippage and calldata size.

The dispatcher enforces the limits at signing:

- The nonce journal is written with each broadcast. A failed journal write refuses every later send until restart, and a sender nonce that moved outside the journal refuses startup.
- A submission with an unknown result keeps its nonce and its fee reservation; a timeout is never read as rejection.
- Actual fees above a reservation are booked first, then a stop is persisted.
- Maintenance transactions (sweeping profit and unwrapping to keep the sender able to pay gas) are limited to two call shapes, run only when the lane is idle and the gate is open, and count against the daily cap.

`live` additionally requires a submission endpoint and a signing secret (ADR 0021). `shadow` runs the same pipeline without signing.

## Consequences

- A misconfigured or runaway instance can lose at most the daily cap in fees plus what the per-asset principal caps expose to a failing route.
- The daily cap also limits winning sends. When it runs out, the instance stops until the next day.
- The hot wallet should hold only what these caps can spend; profit accumulates in the executor until the owner sweeps it.
- Raising a limit on a running instance is an operator change, made in the policy file and recorded with the deployment.
