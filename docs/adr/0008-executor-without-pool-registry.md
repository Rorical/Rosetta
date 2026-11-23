# 8. Executor without an on-chain pool registry

## Status

Accepted

## Context

The first executor kept an on-chain registry of approved pools, pinned code hashes and per-token limits. A route could use only registered pools, so every pool discovered off-chain had to be registered by the owner before it could be traded, and the off-chain pool list had to match the chain entry by entry. Each registry check also cost storage reads on every execution.

The registry protected against a wrong or hostile target. The same protection is available from checks on the outcome of each call, which the contract performs anyway.

## Decision

- The executor holds no pool registry, code-hash pins or token limits. Pool parameters travel in calldata: kind, target, output token, fee and V4 tick spacing for each leg. A leg's input token is the previous leg's output, and the first leg's input is the route's asset.
- Safety comes from four checks that hold for any target:
  - Callback authentication. Before calling a pool, the contract records in transient storage the expected caller, kind and a hash of the callback payload. A callback from any other address, of another kind, or with another payload reverts, and each authorization is consumed once.
  - Minimum output per leg, checked before the contract pays the pool's input.
  - Plan-level inventory. The contract records its balance of every intermediate token at the start and requires the same balance at the end. Each payee (V2 and V3 pools, flash lenders, the V4 manager on settle) checks its own receipt.
  - The final profit check on the asset's actual balance.
- Calldata validity checks need no storage: kind range, native currency only on V4 legs, at most 16 legs, and no price limit on V2-type legs.
- Operator, pause, chain ID, deadline and `configVersion` remain.
- Principal caps, start-asset limits, fee budgets, token exclusions and pool quarantine are enforced off-chain (ADRs 0017 and 0018).

## Consequences

- Any pool the engine discovers can be traded without an owner transaction. Dropping the registry reads cut about 50,000 gas from a successful two-hop route in the contract test suite.
- A wrong or malicious target can at worst make the transaction revert, so the loss is bounded by gas.
- A pool whose code is upgraded is no longer caught by a code-hash pin. A V2 fee taken from calldata is not checked on-chain; the leg's minimum output catches a wrong fee.
- A short-paying token or a pool that underpays is detected only at a later payment, the inventory check or the profit check. The contract cannot report which leg failed, so off-chain diagnosis traces the transaction to attribute it.
