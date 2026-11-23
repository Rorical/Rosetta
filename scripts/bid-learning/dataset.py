"""Join immutable decision features to paid canonical labels, without RPC."""
import hashlib
import json
from collections import Counter
from pathlib import Path

from schema import SCHEMA, checked_features


def hash_key(value):
    # domain.Hash is a Go [32]byte and encodes as a JSON integer array.
    if isinstance(value, list):
        if len(value) != 32:
            raise ValueError("invalid hash length")
        value = "0x" + bytes(value).hex()
    if value == "0x" + "00"*32:
        return ""
    return value


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def read_records(paths, max_records):
    count = 0
    for path in paths:
        # Read a bounded-size snapshot of an append-only journal. An incomplete
        # last line is retried on the next cycle, never treated as a full record.
        with Path(path).open("rb") as stream:
            remaining = Path(path).stat().st_size
            while remaining:
                line = stream.readline(min(remaining, 4 * 1024 * 1024 + 1))
                remaining -= len(line)
                if len(line) > 4 * 1024 * 1024:
                    raise ValueError("journal record exceeds 4 MiB")
                if not line.endswith(b"\n"):
                    if remaining:
                        raise ValueError("oversized journal record")
                    break
                count += 1
                if count > max_records:
                    raise ValueError("journal record limit exceeded")
                yield json.loads(line)


def build_dataset(paths, as_of_ms, max_records=1000000):
    sends, outcomes, voids, diagnoses = {}, {}, {}, {}
    excluded = Counter()
    for record in read_records(paths, max_records):
        kind = record.get("type")
        if kind == "reactive_intent" and record.get("mode") == "live":
            tx = hash_key(record.get("transaction"))
            intent = record.get("intent")
            started = (record.get("submission") or {}).get("StartedAtNS", 0)
            if not tx or not intent or started <= 0 or started // 1000000 > as_of_ms:
                continue
            prior = sends.get(tx)
            if prior and prior["intent"]["ID"] != intent["ID"]:
                raise ValueError("conflicting intent for transaction")
            if not prior or started < prior["submission"]["StartedAtNS"]:
                sends[tx] = record
        elif kind == "settlement":
            receipt = record.get("receipt") or {}
            known = record.get("observed_at_ns", 0)
            if not receipt.get("Basis", "").startswith("canonical_"):
                continue
            if not known:
                excluded["legacy_receipt_without_availability_time"] += 1
                continue
            if known // 1000000 > as_of_ms:
                continue
            tx = hash_key(receipt["Hash"])
            outcomes.setdefault(tx, []).append((receipt, known))
        elif kind == "settlement_voided":
            known = record.get("observed_at_ns", 0)
            if known > 0 and known // 1000000 <= as_of_ms:
                tx = hash_key(record["transaction"])
                voids[tx] = max(voids.get(tx, 0), known)
            elif not known:
                # Unknown-time invalidations exclude the hash conservatively.
                voids[hash_key(record["transaction"])] = as_of_ms * 1000000 + 999999
        elif kind == "revert_diagnosis":
            diagnoses[hash_key(record["transaction"])] = record.get("diagnosis", "")
    rows = []
    for tx, record in sends.items():
        intent = record["intent"]
        bid = intent.get("Bid") or {}
        learning = bid.get("Learning") or {}
        if learning.get("schema") != SCHEMA:
            excluded["missing_decision_features"] += 1
            continue
        started = record["submission"]["StartedAtNS"]
        decision = bid.get("DecisionNS", 0)
        if decision <= 0 or decision > started:
            excluded["invalid_decision_time"] += 1
            continue
        if bid.get("ModelTip") != intent["Call"]["TipCap"]:
            excluded["changed_tip"] += 1
            continue
        features = checked_features(learning["features"])
        if not (0 <= learning["baseline_win"] <= 1 and 0 <= bid.get("WinRaw", -1) <= 1):
            raise ValueError("invalid recorded baseline probability")
        receipt, known = {}, 0
        # Rotated files need not be supplied chronologically. Fold canonical
        # revisions after the latest invalidation, preserving nanosecond order.
        for revision, available in sorted(outcomes.get(tx, []), key=lambda event: event[1]):
            if available <= voids.get(tx, 0):
                continue
            if revision != receipt:
                receipt, known = revision, available
        if known and known < started:
            raise ValueError("canonical label predates submission")
        if known and receipt.get("IntentID") != intent["ID"]:
            raise ValueError("receipt/intent identity mismatch")
        status = "unresolved"
        label, gas, fees, net = None, None, None, None
        if voids.get(tx, 0) >= known and tx in voids:
            status = "voided"
        elif receipt.get("Cancelled"):
            status = "cancelled"
        elif receipt:
            paid = receipt.get("Fees") or {}
            if receipt.get("FeeStatus") or paid.get("total_fee") is None or not paid.get("gas_used"):
                status = "fees_unknown"
            else:
                status = "canonical"
                label = int(receipt["Success"])
                gas, fees = paid["gas_used"], int(paid["total_fee"])
                if gas <= 0 or gas > intent["Call"]["GasLimit"] or fees < 0:
                    raise ValueError("invalid canonical paid cost")
                conversion = intent.get("Conversion", {})
                if not label:
                    net = -fees
                elif receipt.get("Profit") is not None and conversion.get("Numerator", 0) > 0 and conversion.get("Denominator", 0) > 0:
                    net = int(receipt["Profit"]) * int(conversion["Denominator"]) // int(conversion["Numerator"]) - fees
        excluded[status] += 1
        chain = intent["Chain"]["ChainID"]
        steps = intent["Route"]["Steps"]
        scope = {"chain_id": chain, "code_hash": hash_key(intent["Call"]["CodeHash"]),
                 "precheck": any(step.get("PriceBound") is not None for step in steps)}
        pools = sorted(fingerprint(step["Pool"]) for step in steps)
        groups = [f"anchor:{chain}:{hash_key(intent['Version']['Anchor']['Hash'])}"]
        if bid.get("Window"):
            groups.append("opportunity:" + fingerprint([chain, bid["Window"], pools]))
        if receipt.get("Block", {}).get("Hash") and status == "canonical":
            groups.append(f"block:{chain}:{hash_key(receipt['Block']['Hash'])}")
        rows.append({"schema": SCHEMA, "hash": tx, "scope": scope, "groups": groups,
                     "decision_ms": decision // 1000000, "sent_ms": started // 1000000,
                     "known_ms": known // 1000000 if label is not None else None, "features": features,
                     "success": label, "gas_ratio": gas / intent["Call"]["GasLimit"] if gas else None,
                     "delay_ms": (started - decision) / 1000000, "status": status,
                     "baseline_win": learning["baseline_win"], "raw_win": bid["WinRaw"],
                     "fees_wei": fees, "net_wei": net, "diagnosis": diagnoses.get(tx, ""),
                     "policy": learning.get("model_id") or bid.get("CostVersion", "legacy")})
    rows.sort(key=lambda row: (row["decision_ms"], row["hash"]))
    return rows, {"submitted": len(sends), "rows": len(rows), "counts": dict(excluded), "as_of_ms": as_of_ms}


def group_roots(rows):
    """Return connected components of anchor, settlement block and opportunity."""
    parent = {}
    def root(key):
        parent.setdefault(key, key)
        while parent[key] != key:
            parent[key] = parent[parent[key]]
            key = parent[key]
        return key
    for row in rows:
        groups = row["groups"]
        for key in groups[1:]:
            parent[root(key)] = root(groups[0])
    return {row["hash"]: root(row["groups"][0]) for row in rows}


def temporal_split(rows, start_ms, split_ms, end_ms, gap_ms, target="outcome"):
    """Purge overlapping groups and labels unavailable at the training cutoff."""
    def known(row):
        return row["sent_ms"] if target == "delay" else row["known_ms"]
    train = [r for r in rows if start_ms <= r["decision_ms"] < split_ms - gap_ms
             and known(r) is not None and known(r) < split_ms - gap_ms]
    valid = [r for r in rows if split_ms <= r["decision_ms"] < end_ms
             and known(r) is not None and known(r) <= end_ms]
    roots = group_roots(rows)
    held_groups = {roots[r["hash"]] for r in valid}
    train = [r for r in train if roots[r["hash"]] not in held_groups]
    return train, valid
