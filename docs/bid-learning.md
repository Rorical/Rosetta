# GBDT bid learning

An optional, experimental pipeline that trains gradient-boosted trees on the reactive executor's own send history and lets the runtime use them when pricing a tip. It is off by default. Whether it improves net profit over the built-in bid model is `UNVERIFIED`.

The pipeline has three parts:

| Part | Role |
|---|---|
| `scripts/bid-learning/` | Python. Exports labeled datasets from reactive journals, trains with LightGBM 4.6.0, validates on a later window, and publishes model files atomically. |
| `internal/bidmodel` | Go. Loads, validates and evaluates a published model inside the trading process, with no Python or native libraries. |
| `cmd/bidmodel` | Go CLI. Validates an artifact and prints Go predictions for feature vectors, so training can check Go/Python parity before publishing. |

Training never runs in the trading process, and nothing in the pipeline signs, sends or calls an RPC endpoint.

## Models

Each artifact holds three small models over the same features:

| Output | Objective | Use at runtime |
|---|---|---|
| `win` | Binary | Calibrated probability that an executed send at a given tip succeeds. Replaces the built-in win probability for candidate tips. |
| `revert_gas` | Regression | Revert gas as a share of the gas limit, for the expected cost of a failure. |
| `delay` | Quantile (p90) | Remaining time from decision to broadcast. Candidates expected to broadcast after the opportunity expires are dropped. |

Gas used on success still comes from the existing receipt-based estimator. Expected value per candidate tip includes the success fee, the failure fee and the L1 or operator reserve. Tip-dependent pool fees are requoted at the fixed quote state. Per-transaction and daily fee caps, replacement floors, the contract's profit guard and the stop file stay outside the model's control.

Candidate tips are the rival bid breakpoints, 32 points on a logarithmic grid up to the cap, and the cap itself. At most 32 pool requotes run per decision. The search is approximate, and its result is not a global optimum of the tree's curve.

## Features and labels

The `bid-features-v1` schema has 25 numeric inputs in a fixed order shared by `internal/bidmodel/features.go` and `scripts/bid-learning/schema.py`: the built-in raw win probability, log tip, tip over worth, log worth, decision age, fast or full lane, log gas limit, success and revert gas ratios, hop count, funding mode, rival summary counts, legs per protocol, log input amount, log base fee and log extra fee. Amounts enter as `log1p` of base units. No future prices or receipt values are inputs.

Each send records its exact features, the model's content hash, the mode, any fallback reason, and both the baseline and tree predictions. Declined decisions are logged without labels; the exporter never invents outcomes for them.

Labels join sends to canonical receipts by transaction hash:

- `win` and `revert_gas` train only on paid, non-cancelled executions whose signed tip equals the predicted tip.
- `delay` trains on any version with a recorded broadcast time.
- Each canonical record carries `observed_at_ns`, when the outcome became known locally. An outcome observed after a training cutoff never enters that training set.
- Journals that lack features or observation times are excluded, and re-capturing old receipts does not backfill historical observation times.

Models are scoped by chain ID, executor code hash and price-precheck mode. One controller directory serves one scope; mixed journals are rejected.

## Training and publishing

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r scripts/bid-learning/requirements.txt
go build -o bin/bidmodel ./cmd/bidmodel

# Export a labeled dataset for inspection.
.venv/bin/python scripts/bid-learning/main.py export \
  --input 'research/reactive*.jsonl' \
  --output research/bid-dataset.jsonl

# Train and validate once; archive the candidate without activating it.
.venv/bin/python scripts/bid-learning/main.py cycle \
  --input 'research/reactive*.jsonl' \
  --directory state/bid-models \
  --validator bin/bidmodel
```

| Command | Effect |
|---|---|
| `export` | Writes the labeled dataset as JSON lines. `--as-of-ms` fixes the label cutoff. |
| `cycle` | Trains and validates on the latest closed validation slot, archives the artifact and its report. |
| `watch` | Runs `cycle` every `--poll-seconds`. |
| `evaluate` | Scores a published model on a later window without training. |
| `rollback` | Reactivates an earlier approved release. |

`cycle` and `watch` only activate a model with `--promote`, and then only an approved one. A lock file admits one controller per directory.

| Option | Default |
|---|---|
| `--train-hours` | 6 |
| `--validation-minutes` | 30 |
| `--gap-seconds` (between training and validation) | 5 |
| `--settle-seconds` (wait for labels) | 10 |
| `--half-life-minutes` (sample weight decay) | 60 |
| `--ttl-minutes` (model lifetime, at most 1440) | 60 |
| `--min-train`, `--min-valid` | 500, 200 |
| `--max-records` | 1,000,000 |

Validation slots are fixed UTC intervals; each slot is evaluated once and recorded under `cycles/`. Training also needs at least 50 reverts, and at least 30 (training) and 20 (validation) examples of each outcome. Training and validation rows are split by connected groups of anchor, execution block and opportunity, so one opportunity never appears on both sides.

The trees are fixed: depth 3, 8 leaves, 64 rounds, learning rate 0.05, L2 regularization 10, at least 30 rows per leaf. Hyperparameters are never tuned on the validation set.

A candidate is approved only when all of these hold on the validation window:

- Prediction support covers at least 70% of rows.
- Across at least 30 independent groups, the p95 of the bootstrap Brier-score difference against the baseline is below zero.
- Log loss is no worse than the raw and calibrated built-in models and any incumbent model.
- With at least 20 supported reverts, revert-gas MSE and underpredicted fee are no worse than the existing estimate.
- The delay model's p90 pinball loss beats a constant p90 from training, with at least 80% coverage.

These are engineering gates, not evidence of profit. The report's net figures describe actions actually taken, not what a different tip would have earned.

Artifacts are archived as `releases/<sha256>.json` with a `releases/<sha256>.report.json` report. Before archiving, `cmd/bidmodel` predicts on the last 256 training vectors and the support boundaries, and the Go and Python outputs must match. Activation atomically replaces `active.json`.

```bash
./bin/bidmodel -model state/bid-models/active.json -chain 10 -code-hash 0x... [-precheck]
./bin/bidmodel -model state/bid-models/active.json -chain 10 -code-hash 0x... -predict < vectors.jsonl
```

To return to an earlier model:

```bash
.venv/bin/python scripts/bid-learning/main.py rollback \
  --directory state/bid-models --release <sha256> --validator bin/bidmodel
```

Rollback accepts only an approved, unexpired release whose content hash matches, and it does not extend the release's lifetime.

To score a published model on an independent later window:

```bash
.venv/bin/python scripts/bid-learning/main.py evaluate \
  --input 'research/reactive*.jsonl' \
  --model state/bid-models/releases/<sha256>.json \
  --since-ms <start> --until-ms <end> \
  --output research/bid-independent-report.json
```

The window must start after the model was created and end within its lifetime.

## Runtime modes

The reactive configuration selects the mode:

```json
{
  "bid_model": "apply",
  "bid_tree_mode": "shadow",
  "bid_tree_file": "state/bid-models/active.json"
}
```

| `bid_tree_mode` | Behavior |
|---|---|
| `off` (default) | The tree is not loaded. |
| `shadow` | The built-in model chooses the tip; the tree's prediction for the same action is recorded alongside it. Adds no requotes. |
| `apply` | The tree's predictions choose the tip. Only approved models are used. |

Both `shadow` and `apply` need `bid_model: apply`. To inspect an unapproved candidate in shadow, point `bid_tree_file` at its `releases/<sha256>.json`.

The runtime reloads the file every 10 seconds in the background. Loading checks the schema, feature order, execution scope, training and validation order, lifetime, numeric ranges and tree topology, and rejects files over 8 MiB, more than 128 trees per model, or more than 127 nodes per tree. Each decision uses one immutable snapshot. A corrupt new file keeps the previous model until that model expires. A missing, expired or unapproved model, or a candidate outside the training support, falls back to the built-in model, and the reason is recorded with the prediction.

To stop using trees immediately, create the stop file, set `bid_tree_mode` to `off`, and restart.

## Checks

```bash
make check-bid-learning BID_PYTHON=.venv/bin/python
```

This builds `cmd/bidmodel`, runs the Python unit tests in `scripts/bid-learning`, and runs the Go race tests for `internal/bidmodel`, `internal/planning`, `internal/remote` and `cmd/bidmodel`. The tests cover label timing, group leakage, unknown and voided outcomes, independence of fitted trees from later labels, LightGBM/Go parity (`internal/bidmodel/testdata/lightgbm-4.6.0.json`, a synthetic vector set), atomic publish and rollback, expiry and scope rejection, shadow behavior, and candidate cost accounting. Synthetic training cases check the pipeline only; they say nothing about returns.
