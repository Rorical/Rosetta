# 19. Send lanes, nonce journal and racing variants

## Status

Accepted

## Context

A sender has one nonce sequence. While a transaction with nonce `n` is unresolved, the next transaction cannot land first. A conditional send (ADR 0013) returns only after the sequencer executes it, which holds the sender for a couple of hundred milliseconds; with one sender, opportunities arriving in that time are lost.

A crash between signing and recording a send can leave the process unaware of a transaction that may still land. A second transaction signed with the same nonce would then replace or collide with it, and the budget would not reflect it.

One opportunity often has several viable routes. Sending only the best one loses the opportunity when that route fails for its own reasons, and sending several from one sender is impossible without nonce ordering.

## Decision

- The primary sender is lane 0. `lanes[]` adds senders, each with its own signing secret, nonce journal and dispatcher, and each must be an executor operator. An extra lane takes work only while its balance covers a transaction budget.
- The send queue has one slot per lane. A new intent that shares pools with a queued one replaces it only if better; with every slot full it must beat the worst. A pool already in flight on one lane is not sent again on another, and an intent is not built on a view that does not yet show our own in-flight transaction.
- Wallet-funded routes, cancellations, maintenance and replacements stay on lane 0. Intents moving to another lane are resealed for that sender.
- Each dispatcher journals the signed bytes with each broadcast. A lane's nonce is released as soon as any signed variant of it appears in an accepted stream prefix; its fee reservation stays until the receipt is booked.
- With a single lane, the same nonce may be replaced up to three times by an intent whose conservative net beats the previous one by `replacement_min_gain_bps`. Every signed variant is kept for reconciliation, and replacement stops once our hash is seen. With extra lanes, a new intent takes a free lane instead.
- When every known variant of a pending nonce has lost its profit and a cancel costs less than the modelled failure, a zero-value self-transfer at the same nonce is sent once. Its cost is booked separately from arbitrage results.
- Variants of one opportunity race on idle lanes as a group. Routes that share a held pool with identical condition values are mutually exclusive by construction: the first to execute moves the pool and the others are dropped free. When that is not enough, hot routes claim the opportunity through the executor's `claimEpoch`: each holds the current value as a condition, and the first to succeed increments it. Variants yield their lanes to distinct opportunities first.

## Consequences

- Throughput scales with the number of funded senders, at the price of more keys, more operator registrations and a daily budget split across lanes.
- A crash leaves journals that a restart reconciles before any new signature.
- Releasing nonces on stream inclusion lets the next send go out before the receipt, so the budget must count the released transaction as still open.
- A successful claim pays one extra cold storage write.
- Replacement acceptance and timing depend on the sequencer. A replacement sent before our original is visible in the stream can be refused by nonce without charge.
