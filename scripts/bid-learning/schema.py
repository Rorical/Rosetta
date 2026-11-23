"""The versioned decision-time contract shared with internal/bidmodel."""
import math

SCHEMA = "bid-features-v1"
FEATURES = [
    "raw_win", "log_tip", "tip_worth", "log_worth", "age_ms", "fast",
    "log_gas_limit", "success_gas_ratio", "revert_gas_ratio", "hops", "funding",
    "first", "unknown", "log_windows", "log_route_windows", "v2", "v3", "v4",
    "camelot_v3", "slipstream", "velodrome_v2", "solidly_v3", "log_input",
    "log_base_fee", "log_extra_fee",
]


def checked_features(values):
    if len(values) != len(FEATURES) or any(not math.isfinite(v) for v in values):
        raise ValueError("invalid decision feature vector")
    return values


def ensemble_predict(ensemble, features):
    total = 0.0
    for tree in ensemble["trees"]:
        index = 0
        while tree[index]["feature"] >= 0:
            node = tree[index]
            index = node["left"] if features[node["feature"]] <= node["threshold"] else node["right"]
        total += tree[index]["value"]
    return total


def predict(artifact, features):
    """Match Go inference, including its supported-domain fallback."""
    checked_features(features)
    if any(v < lo or v > hi for v, lo, hi in zip(features, artifact["low"], artifact["high"])):
        return None
    margin = ensemble_predict(artifact["win"], features)
    win = 1 / (1 + math.exp(-max(-700, min(700, margin))))
    gas = ensemble_predict(artifact["revert_gas"], features)
    delay = ensemble_predict(artifact["delay"], features)
    if not (0 < gas <= 1 and 0 <= delay <= 2000):
        return None
    return {"win": win, "revert_ratio": gas, "delay_ms": delay}
