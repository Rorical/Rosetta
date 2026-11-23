// Package exec builds and signs transactions, manages nonces and conditional
// submission, and tracks receipts. It is the only package allowed to sign or
// send transactions.
//
// Executor calldata, call envelopes, budget-based economics and versioned ready
// plans with dispatch validation are implemented. Signing, nonce management and
// submission use a separately admitted ReactiveIntent for unconfirmed execution.
// Confirmed ReadyPlan semantics are unchanged.
package exec
