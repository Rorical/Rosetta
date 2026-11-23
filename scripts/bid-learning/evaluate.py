"""Scores are for the observed actions; they do not estimate counterfactual P&L."""
import math
import numpy as np

from schema import predict
from dataset import group_roots


def probability_scores(predicted, target):
    p = np.clip(np.asarray(predicted), 1e-12, 1 - 1e-12)
    y = np.asarray(target)
    return {"brier": float(np.mean((p-y)**2)),
            "log_loss": float(np.mean(-y*np.log(p)-(1-y)*np.log1p(-p))),
            "mean_probability": float(np.mean(p)), "success_rate": float(np.mean(y))}


def evaluate(artifact, rows, incumbent=None):
    y = np.array([r["success"] for r in rows])
    forecasts = [predict(artifact, r["features"]) for r in rows]
    covered = [i for i, p in enumerate(forecasts) if p is not None]
    predicted = [p["win"] if p else r["baseline_win"] for p, r in zip(forecasts, rows)]
    comparators = {"raw": [r["raw_win"] for r in rows], "baseline": [r["baseline_win"] for r in rows]}
    if incumbent:
        if incumbent["scope"] != artifact["scope"]:
            raise ValueError("incumbent scope mismatch")
        # Only compare an incumbent trained before this validation window.
        if incumbent["validation_until_ms"] > artifact["train_until_ms"]:
            raise ValueError("incumbent overlaps candidate validation window")
        comparators["incumbent"] = [(predict(incumbent, r["features"]) or {"win": r["baseline_win"]})["win"] for r in rows]
    scores = {name: probability_scores(values, y) for name, values in comparators.items()}
    scores["candidate"] = probability_scores(predicted, y)
    # Resample whole anchor groups. The gate tests the best observed comparator,
    # chosen conservatively, without selecting candidate hyperparameters here.
    reference = min(comparators, key=lambda name: scores[name]["brier"])
    delta = (np.array(predicted)-y)**2 - (np.array(comparators[reference])-y)**2
    clusters = {}
    roots = group_roots(rows)
    for i, row in enumerate(rows):
        clusters.setdefault(roots[row["hash"]], []).append(i)
    cluster_values = list(clusters.values())
    rng = np.random.default_rng(17)
    boot = []
    for _ in range(300):
        sample = np.concatenate([cluster_values[i] for i in rng.integers(0, len(cluster_values), len(cluster_values))])
        boot.append(float(np.mean(delta[sample])))
    reverts = [i for i in covered if not y[i]]
    gas = {"count": len(reverts)}
    if reverts:
        truth = np.array([rows[i]["gas_ratio"] for i in reverts])
        candidate = np.array([forecasts[i]["revert_ratio"] for i in reverts])
        baseline = np.array([rows[i]["features"][8] for i in reverts])
        # L2 cost sensitivity, using decision-time tip/base and gas limit only.
        sensitivity = np.array([math.expm1(rows[i]["features"][6]) *
                                (math.expm1(rows[i]["features"][1]) + math.expm1(rows[i]["features"][23])) for i in reverts])
        gas.update(candidate_mse=float(np.mean((truth-candidate)**2)),
                   baseline_mse=float(np.mean((truth-baseline)**2)),
                   candidate_underprediction_wei=float(np.sum(np.maximum(truth-candidate, 0)*sensitivity)),
                   baseline_underprediction_wei=float(np.sum(np.maximum(truth-baseline, 0)*sensitivity)))
    delay = {"count": len(covered)}
    if covered:
        truth = np.array([rows[i]["delay_ms"] for i in covered])
        predicted_delay = np.array([forecasts[i]["delay_ms"] for i in covered])
        residual = truth-predicted_delay
        delay.update(pinball_p90=float(np.mean(np.maximum(.9*residual, -.1*residual))),
                     coverage=float(np.mean(truth <= predicted_delay)))
    lanes = {}
    for lane, flag in [("full", 0), ("fast", 1)]:
        indexes = [i for i, r in enumerate(rows) if r["features"][5] == flag]
        if indexes:
            lanes[lane] = {"count": len(indexes), **probability_scores([predicted[i] for i in indexes], y[indexes])}
    return {"basis": "held_out_observed_actions_only; no_counterfactual_profit", "count": len(rows),
            "groups": len(clusters), "coverage": len(covered)/len(rows), "scores": scores,
            "brier_delta_p95": float(np.quantile(boot, .95)), "reference": reference,
            "revert_gas": gas, "delay": delay, "lanes": lanes,
            "observed_paid_wei": sum(r["fees_wei"] for r in rows),
            "observed_net_wei": sum(r["net_wei"] for r in rows if r["net_wei"] is not None),
            "net_known": sum(r["net_wei"] is not None for r in rows)}


def acceptance(report, delay_baseline_loss):
    reasons = []
    candidate = report["scores"]["candidate"]
    if report["coverage"] < .7:
        reasons.append("support_coverage_below_70_percent")
    if report["groups"] < 30 or report["brier_delta_p95"] >= 0:
        reasons.append("no_grouped_brier_improvement")
    for name, score in report["scores"].items():
        if name != "candidate" and candidate["log_loss"] > score["log_loss"]:
            reasons.append("log_loss_worse_than_" + name)
    gas = report["revert_gas"]
    if gas["count"] < 20:
        reasons.append("too_few_supported_reverts")
    elif gas["candidate_mse"] > gas["baseline_mse"] or gas["candidate_underprediction_wei"] > gas["baseline_underprediction_wei"]:
        reasons.append("revert_cost_worse_than_baseline")
    delay = report["delay"]
    if not delay["count"] or delay["pinball_p90"] > delay_baseline_loss or delay["coverage"] < .8:
        reasons.append("delay_validation_failed")
    return reasons


def evaluate_delay(artifact, rows, baseline):
    pairs = [(row, predict(artifact, row["features"])) for row in rows]
    pairs = [(row, forecast) for row, forecast in pairs if forecast is not None]
    report = {"count": len(pairs), "available": len(rows)}
    if not pairs:
        return report
    truth = np.array([row["delay_ms"] for row, _ in pairs])
    residual = truth-np.array([forecast["delay_ms"] for _, forecast in pairs])
    baseline_residual = truth-baseline
    report.update(pinball_p90=float(np.mean(np.maximum(.9*residual, -.1*residual))),
                  coverage=float(np.mean(residual <= 0)),
                  baseline_pinball=float(np.mean(np.maximum(.9*baseline_residual, -.1*baseline_residual))))
    return report
