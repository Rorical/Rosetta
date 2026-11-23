# 2. Chain families and chain identity

## Status

Accepted

## Context

Two L2 stacks matter for this engine, and they disagree on the details that decide whether a transaction is profitable:

- Nitro charges the L1 data cost as extra gas inside the transaction's own gas (poster gas). OP Stack charges an L1 data fee and an operator fee on top of L2 execution gas.
- Ordering is a chain owner's choice. Nitro chains can order by priority fee or by arrival; OP Stack chains order by priority fee.
- Pre-confirmation streams, conditional-transaction checks and receipt formats differ by stack.

A chain ID says which chain a process talks to. It does not say how that chain charges fees or orders transactions. Reusing one stack's code for another by changing the chain ID would produce plans whose fee proofs and bids are wrong in ways tests on the original chain would not catch.

## Decision

- The engine supports two families: `nitro` and `op-stack`. Any other family, including `ethereum`, is rejected before a backend is created.
- A network profile names the family, chain ID, parent chain ID, endpoints, ordering and DEX deployments. Built-in profiles cover Arbitrum One and other Nitro chains, and OP Mainnet, Base, Unichain and Ink. A custom profile is strict JSON; unknown fields and trailing data are errors.
- The parent chain ID must be non-zero and differ from the chain ID. Nitro profiles must state their ordering; OP Stack defaults to priority.
- Identity is checked at every boundary that can see it: the RPC chain ID at startup, the feed handshake when the server sends its headers, the executor contract (which stores `block.chainid` at deployment and checks it on every call), ready-plan digests, and dispatch, where the caller must pass the configured chain.
- Fee models, confirmation evidence and send features are selected by family. OP evidence must match the OP fee model, and the Nitro fee model refuses OP evidence. Conditional sends are enabled only for Nitro.
- On arrival-ordered chains the planner does not bid a priority tip.
- DEX deployments belong to the profile. A protocol not configured for a chain is not discovered there; addresses are never borrowed from another chain.

## Consequences

- Adding a chain within a supported family is a profile plus its DEX deployments.
- Adding a family requires its own fee model, confirmation adapter, submission semantics and tests. Until those exist, a chain of that family cannot run.
- The same pool address on two chains is two different pools, because each process holds only its own chain's state.
