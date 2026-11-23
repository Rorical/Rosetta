"""Bounded rolling retraining, with a later untouched validation interval."""
import hashlib
import json
import numpy as np
import lightgbm as lgb

from dataset import temporal_split
from evaluate import acceptance, evaluate, evaluate_delay
from schema import FEATURES, SCHEMA, ensemble_predict


def export_ensemble(booster):
    trees = []
    for source in booster.dump_model()["tree_info"]:
        nodes = []
        def visit(node):
            index = len(nodes)
            entry = {"feature": -1, "threshold": 0., "left": 0, "right": 0, "value": 0.}
            nodes.append(entry)
            if "leaf_value" in node:
                entry["value"] = node["leaf_value"]
            else:
                if node["decision_type"] != "<=" or node.get("missing_type", "None") != "None":
                    raise ValueError("export accepts finite numeric splits only")
                entry.update(feature=node["split_feature"], threshold=node["threshold"],
                             left=visit(node["left_child"]), right=visit(node["right_child"]))
            return index
        visit(source["tree_structure"])
        trees.append(nodes)
    return {"trees": trees}


def train_candidate(rows, *, start_ms, split_ms, end_ms, created_ms, gap_ms=5000,
                    min_train=500, min_valid=200, ttl_ms=3600000, half_life_ms=3600000,
                    incumbent=None):
    if not (start_ms < split_ms - gap_ms < split_ms < end_ms <= created_ms) or not (0 < ttl_ms <= 86400000) or half_life_ms <= 0:
        raise ValueError("invalid temporal training configuration")
    train, valid = temporal_split(rows, start_ms, split_ms, end_ms, gap_ms)
    delay_train, delay_valid = temporal_split(rows, start_ms, split_ms, end_ms, gap_ms, target="delay")
    counts = {"train": len(train), "validation": len(valid), "train_reverts": sum(r["success"] == 0 for r in train)}
    if len(train) < min_train or len(valid) < min_valid or counts["train_reverts"] < 50:
        return None, {"approved": False, "reasons": ["insufficient_samples"], "counts": counts}
    if min(sum(r["success"] == label for r in train) for label in [0, 1]) < 30 or min(sum(r["success"] == label for r in valid) for label in [0, 1]) < 20:
        return None, {"approved": False, "reasons": ["insufficient_class_support"], "counts": counts}
    scope = train[0]["scope"]
    if any(row["schema"] != SCHEMA or row["scope"] != scope for row in train + valid + delay_train + delay_valid):
        raise ValueError("mixed feature or execution scopes")
    x = np.asarray([r["features"] for r in train])
    y = np.asarray([r["success"] for r in train])
    weights = np.exp2((np.array([r["decision_ms"] for r in train])-split_ms)/half_life_ms)
    params = dict(boosting_type="gbdt", max_depth=3, num_leaves=8, learning_rate=.05,
                  min_data_in_leaf=30, lambda_l2=10., verbosity=-1, seed=17,
                  deterministic=True, force_col_wise=True, num_threads=1,
                  use_missing=False, zero_as_missing=False)
    def fit(features, labels, sample_weights, objective):
        model = lgb.train({**params, "objective": objective, "alpha": .9},
                          lgb.Dataset(features, label=labels, weight=sample_weights,
                                      feature_name=FEATURES), num_boost_round=64)
        portable = export_ensemble(model)
        reference = model.predict(features, raw_score=True)
        exported = np.array([ensemble_predict(portable, row) for row in features])
        if not np.allclose(reference, exported, rtol=1e-12, atol=1e-12):
            raise ValueError("portable tree inference differs from LightGBM")
        return portable
    delay_x = np.asarray([r["features"] for r in delay_train])
    delay_weights = np.exp2((np.array([r["decision_ms"] for r in delay_train])-split_ms)/half_life_ms)
    reverted = y == 0
    artifact = {"schema": SCHEMA, "features": FEATURES, "scope": scope,
                "train_until_ms": split_ms, "validation_until_ms": end_ms,
                "created_ms": created_ms, "expires_ms": created_ms + ttl_ms,
                "approved": False, "training_rows": len(train), "validation_rows": len(valid),
                "low": x.min(axis=0).tolist(), "high": x.max(axis=0).tolist(),
                "win": fit(x, y, weights, "binary"),
                "revert_gas": fit(x[reverted], np.array([r["gas_ratio"] for r in train])[reverted], weights[reverted], "regression"),
                "delay": fit(delay_x, np.array([r["delay_ms"] for r in delay_train]), delay_weights, "quantile")}
    # Counts change over time even with identical opportunity support. These
    # bounded context fields do not define a new tip/value/route domain.
    for index in [0, 7, 8, 11, 12]:
        artifact["low"][index], artifact["high"][index] = 0., 1.
    for index in [13, 14]:
        artifact["low"][index], artifact["high"][index] = 0., float(np.log1p(6144))
    report = evaluate(artifact, valid, incumbent)
    constant = float(np.quantile([r["delay_ms"] for r in delay_train], .9))
    report["delay"] = evaluate_delay(artifact, delay_valid, constant)
    delay_loss = report["delay"].get("baseline_pinball", 0.)
    reasons = acceptance(report, delay_loss)
    artifact["approved"] = not reasons
    report.update(approved=not reasons, reasons=reasons, counts=counts,
                  delay_baseline_p90_ms=constant, delay_baseline_pinball=delay_loss,
                  dataset_sha256=hashlib.sha256(json.dumps({"outcomes": train+valid, "delays": delay_train+delay_valid}, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
                  windows={"start_ms": start_ms, "split_ms": split_ms, "end_ms": end_ms, "gap_ms": gap_ms},
                  learner={"library": "lightgbm", "version": lgb.__version__, "parameters": params, "rounds": 64,
                           "half_life_ms": half_life_ms})
    return artifact, report
