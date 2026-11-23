# 16. GBDT bid learning, off by default

## Status

Accepted

## Context

The bidding model (ADR 0015) is a hand-built structure with a few calibrated terms. It cannot use features such as route shape, protocol mix, funding mode or base fee except through its fixed form. Gradient-boosted trees can, but a learned model in the send path brings risks the hand-built one does not: training on outcomes that were not yet known at decision time, overfitting to a short window, silently drifting, and running inference code that differs from the training code.

Outcomes are also censored. The engine observes the result only of the tip it sent, so no dataset shows what a different tip would have done.

## Decision

- Three small LightGBM models are trained outside the trading process: win probability given execution, revert gas as a fraction of the limit, and the 90th percentile of time from decision to broadcast. Pool fees, funding costs and budgets stay deterministic.
- Training runs in `scripts/bid-learning` (Python). The trading process only evaluates published artifacts in Go (`internal/bidmodel`); no Python, native library or training code enters it.
- Each decision seals its feature vector, the baseline prediction and the tree prediction with the intent. Labels join by transaction hash to canonical receipts and carry the time they became available locally. Cancelled, superseded, fee-unknown and unincluded variants are not trained as failures.
- Training and validation are split by label availability time with a gap, and by connected groups of anchor block, execution block and opportunity, so one event never appears on both sides. Hyperparameters are fixed in advance.
- A candidate is published only if it beats the raw and calibrated baselines (and the active model, if any) on grouped bootstrap Brier and log loss, covers enough of the decision space, and does not underestimate failure cost. Go and Python inference must agree on the archived artifact. Replacing the active model requires an explicit `--promote`.
- At runtime `bid_tree_mode` is `off` (default), `shadow` or `apply`, and anything but `off` requires `bid_model=apply` and `bid_tree_file`. One snapshot serves a whole decision. An expired, mismatched, unapproved or out-of-support model falls back to the baseline. Models are scoped by chain ID, executor code hash and precheck mode.
- With a tree model active, tip search adds 32 log-spaced grid points to the rival breakpoints, with a bounded number of pool requotes.

## Consequences

- The learned path can be evaluated in shadow on live decisions without affecting what is signed.
- Fee caps, the stop file, replacement rules and the contract's profit check do not depend on the model.
- Observational labels cannot prove the payoff of tips that were never tried, so publication gates measure predictive fit only. Enabling `apply` needs a controlled live window and stays an operator decision.
- The training pipeline is a second language and a second set of checks (`make check-bid-learning`).
