# Architecture decision records

These records explain the design decisions a contributor needs to understand before changing Rosetta: why the engine is shaped the way it is, what each choice costs, and which constraints a change must keep. Each record follows Michael Nygard's format (context, decision, consequences) and describes the decision as the code implements it. A record is never rewritten to reverse its decision; a new record supersedes it, and the old one's status points to the replacement. Add a record when a change alters architecture, the execution path or a safety property, and number it after the highest existing one.

| Number | Title | Status |
|---|---|---|
| [0001](0001-one-process-per-chain-rpc-backend.md) | One process per chain on an RPC backend | Accepted |
| [0002](0002-chain-families-and-identity.md) | Chain families and chain identity | Accepted |
| [0003](0003-canonical-state-and-speculative-branches.md) | Canonical state from whole blocks, speculative branches for pre-confirmation data | Accepted |
| [0004](0004-deterministic-state-transitions.md) | Deterministic state transitions | Accepted |
| [0005](0005-protocol-adapters-and-pool-identity.md) | Protocol adapters and pool identity | Accepted |
| [0006](0006-staged-cycle-search.md) | Staged search over a precomputed cycle table | Accepted |
| [0007](0007-single-transaction-executor.md) | Atomic execution in one executor transaction | Accepted |
| [0008](0008-executor-without-pool-registry.md) | Executor without an on-chain pool registry | Accepted |
| [0009](0009-funding-modes.md) | Funding modes | Accepted |
| [0010](0010-ready-plans-and-reactive-intents.md) | Ready plans and reactive intents | Accepted |
| [0011](0011-fee-budgets-per-chain-family.md) | Fee budgets per chain family | Accepted |
| [0012](0012-early-revert-price-bounds.md) | Early revert on moved pools | Accepted |
| [0013](0013-conditional-transactions-on-quiet-pools.md) | Conditional transactions on quiet pools | Accepted |
| [0014](0014-condition-words-derived-from-events.md) | Condition storage words derived from pool events | Accepted |
| [0015](0015-bidding-model.md) | Bidding by expected profit over a win-probability model | Accepted |
| [0016](0016-gbdt-bid-learning-off-by-default.md) | GBDT bid learning, off by default | Accepted |
| [0017](0017-token-risk-and-pool-quarantine.md) | Token risk registry and pool quarantine without allowlists | Accepted |
| [0018](0018-mandatory-live-safety-limits.md) | Mandatory live safety limits | Accepted |
| [0019](0019-send-lanes-and-nonce-journal.md) | Send lanes, nonce journal and racing variants | Accepted |
| [0020](0020-submission-isolated-from-read-pacing.md) | Submission isolated from read pacing | Accepted |
| [0021](0021-signing-only-in-exec.md) | Signing only in `internal/exec`, with keys from a secret manager or an explicit keystore | Accepted |
| [0022](0022-console-holds-no-keys.md) | Operator console holds no keys and gates only at delivery | Accepted |
| [0023](0023-configuration-via-flags.md) | Configuration through flags and files, environment only for secrets | Accepted |
| [0024](0024-offline-tests-with-chain-fixtures.md) | Offline tests against recorded chain data and a native OP reference | Accepted |
