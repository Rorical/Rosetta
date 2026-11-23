# 11. Fee budgets per chain family

## Status

Accepted

## Context

An opportunity is profitable only after every fee the sender pays. The two supported families charge differently:

- Nitro adds the L1 data cost as poster gas inside the transaction's gas, priced from `ArbGasInfo.getPricesInWei`. The receipt's gas used already includes it.
- OP Stack charges L2 execution gas, an L1 data fee computed from the compressed signed transaction, and an operator fee, each by its own formula. The GasPriceOracle quotes the L1 and operator parts.

A budget that ignores the data fee approves losing trades, and a budget built from one family's formula is wrong on the other. Fee parameters also move between quote and inclusion. A fee read on the send path costs a round trip the opportunity rarely survives.

## Decision

- Fee models are typed per family. `exec.NitroFees` keeps poster gas inside transaction gas and refuses OP evidence. `exec.OPFees` keeps L2 gas, the L1 data fee and the operator fee separate and refuses Nitro context. Ready-plan evidence names its accounting mode, and dispatch re-checks it against the configured family.
- Full confirmation obtains the whole fee before encoding `minProfit`, and rejects a plan when the oracle method is missing. A missing quote never defaults to zero.
- The OP L1 size bound covers the signature and worst-case FastLZ expansion, so it sits above the oracle's practical bound for any calldata.
- Reactive budgets use cached fee parameters:
  - The fee cap is the cached base fee times `base_fee_headroom_bps`. A transaction that would need more is refused by the sequencer without charge.
  - Nitro: poster gas is computed from the cached L1 price at the route's uncompressed calldata bound with twofold headroom, and added to the gas limit.
  - OP Stack: the L1 and operator fees are sampled at two sizes and interpolated linearly, rounding up, with twofold headroom.
  - The budget is priced on expected gas. Expected gas is the route's own observed p75 once it has receipts, then the median of similar routes, then `gas_prior_bps` of the limit. The limit is `route_gas_base + route_gas_per_step × legs`, so a long route does not fail for lack of gas.
- The fee reserved for the expected gas is converted into the start asset at the same state as the quote and added to the contract's `minProfit`, so an on-chain success also covers that fee.
- The daily and per-transaction caps (ADR 0018) reserve the maximum possible fee of every signed transaction until it settles.

## Consequences

- An intent whose gross does not cover its cheapest budget is rejected before bidding, and skip statistics record how close it came.
- Headroom and expected-gas estimates trade missed opportunities against overpaying. Too much headroom inflates `minProfit` and rejects real opportunities; too little lets base-fee spikes refuse sends.
- A new family needs its own fee model and evidence type before any route on it can pass admission.
