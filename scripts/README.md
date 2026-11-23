# Scripts

Python tools for offline maintenance, analysis and model training. None of them runs inside the trading process, signs a transaction or reads a key.

| Path | Purpose |
|---|---|
| `paper/maintain.py` | Maintain a `-run-root` directory: request a rotation over the control socket (`rotate`), promote a finished run's `final-pools.json` to `resume.json` (`finish`), and compress closed runs while pruning old replays to a storage budget (`archive`). Makes no RPC calls and never touches a run still being written. |
| `paper/audit.py` | Run `routeaudit` over closed paper runs using each run's saved search configuration. |
| `bid-learning/` | Export labeled datasets from reactive journals, train and validate LightGBM bid models, and publish them atomically. See [bid-learning.md](../docs/bid-learning.md). |
| `prepare-executor.py` | Build unsigned deployment and operator transactions for the executor from a Foundry artifact and a setup file. Uses `cast`; no RPC. |

Usage:

```bash
python3 scripts/paper/maintain.py rotate  "$STATE"
python3 scripts/paper/maintain.py finish  "$STATE"
python3 scripts/paper/maintain.py archive "$STATE" --keep-hours 24 --max-replay-bytes 8589934592
python3 scripts/paper/audit.py "$STATE" --routeaudit bin/routeaudit

python3 scripts/prepare-executor.py \
  --artifact contracts/out/ArbitrageExecutor.sol/ArbitrageExecutor.json \
  --setup setup.json --out unsigned-bundle.json
```

The run-root layout is described in [configuration.md](../docs/configuration.md#run-directories). Keep setup files and generated bundles outside the repository or in an ignored directory.

Tests:

```bash
python3 -m unittest discover -s scripts/paper -p 'test_*.py'
make check-bid-learning BID_PYTHON=.venv/bin/python
```
