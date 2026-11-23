# 13. Conditional transactions on quiet pools

## Status

Accepted

## Context

A price-bound revert (ADR 0012) still costs gas at the bid tip. On Nitro, `eth_sendRawTransactionConditional` accepts expected storage values per account and a `timestampMax`. The sequencer checks them when it admits the transaction, against a state at least about two seconds old as well as the latest, and again at the transaction's execution position. A failed check returns `-32003`; the transaction is dropped without charge and its nonce is not consumed.

So a condition on a pool that a rival would move turns a lost race into a free drop. The pool that triggered the opportunity cannot be held, because it changed within the last two seconds and would fail the admission check. The other pools on the route usually have not changed and are moved by a rival taking the same route.

OP Stack documents a different check (against the parent of the latest block at admission); whether a failed check at block building is also free there is unverified.

## Decision

- Conditional sends are enabled only on Nitro chains (`hot_path`, `packed_conditions`). Configuration on an OP Stack chain is rejected.
- A pool is quiet when our state shows no change for at least 12 blocks. A route holds each quiet pool by conditions on the storage words that carry its price and active liquidity (the reserves word for a V2 pair). Storage roots are not used: fee collection and flash loans change the root without changing the price, and would void winning transactions.
- Word layouts are found per pool by matching our state against its leading storage slots, since forks place the same fields in different slots. V4 pools use computed PoolManager slots, and several V4 pools on one manager share one account entry.
- Every leg is either held by a condition or bounded by a price check. An intent with a leg that has neither is rejected.
- Hot-path routes also hold the executor's slot 0 (unpaused and the current `configVersion`) and carry `timestampMax` equal to the deadline, so a pause, an owner change or expiry drops them free. Packed routes keep their in-contract pause and price checks and add pool conditions.
- Storage words are read off the send path, and only cached when they equal our state; condition words then follow pool events (ADR 0014).
- A conditional rejection holds the route's pools off for up to two seconds, until the stream shows them change, so the next build does not resend the same losing route without conditions.
- Conditional sends return only after execution, so sending runs on dedicated lane workers (ADR 0019).

## Consequences

- When a rival takes the same route first and touches a held pool, the loss is free, and the bid model can bid higher on such routes (ADR 0015).
- A rival that only reverses the trigger pool still meets the price bound and costs that revert's gas.
- A word that is wrong or stale only drops the transaction free, so errors in layout discovery cost missed sends and never funds.
- Some unrelated writes void a held word: staking changes to Slipstream liquidity, for example, cause free false rejections.
- The conditional path depends on one sequencer behaviour. A change in Nitro's check semantics would need re-measurement before conditions are trusted again.
