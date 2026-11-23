import copy
import json
import math
from pathlib import Path
import tempfile
import subprocess
import sys
import time
import unittest


from dataset import build_dataset, temporal_split
from publish import archive, encoded, rollback
from schema import FEATURES, SCHEMA, predict
from train import train_candidate


SCOPE = {"chain_id": 10, "code_hash": "0x" + "11"*32, "precheck": True}


def examples():
    now = time.time_ns()//1000000
    split, end = now-600000, now-1000
    rows = []
    for i in range(1200):
        index = i % 20
        f = [0.] * len(FEATURES)
        f[0], f[1], f[2], f[3] = .5, math.log1p(100+index), (100+index)/1000, math.log1p(1000)
        f[4], f[5], f[6], f[7], f[8] = 20+index, float(index % 2), math.log1p(1000000), .5, .01
        f[9], f[10], f[11], f[12], f[13], f[14] = 2., 1., .1, .05, 4., 3.
        f[16], f[19], f[22], f[23], f[24] = 1., 1., 20., 2., 8.
        at = split-1000000 + i*1000 if i < 800 else split+(i-800)*1000
        rows.append({"schema": SCHEMA, "scope": SCOPE, "hash": str(i), "groups": ["group:"+str(i//2)],
                     "decision_ms": at, "sent_ms": at+50, "known_ms": at+100,
                     "features": f, "success": int(index >= 10), "gas_ratio": .03+index*.005,
                     "delay_ms": 30+index, "baseline_win": .5, "raw_win": .5,
                     "fees_wei": 1000, "net_wei": 100, "status": "canonical"})
    return rows, split, end, now


class TemporalTests(unittest.TestCase):
    def test_late_labels_and_transitive_groups_do_not_leak(self):
        rows = [dict(hash="a", decision_ms=10, sent_ms=11, known_ms=20, groups=["a", "link"]),
                dict(hash="b", decision_ms=30, sent_ms=31, known_ms=110, groups=["b"]),
                dict(hash="bridge", decision_ms=95, sent_ms=96, known_ms=None, groups=["link", "c"]),
                dict(hash="c", decision_ms=120, sent_ms=121, known_ms=130, groups=["c"]),
                dict(hash="d", decision_ms=40, sent_ms=41, known_ms=50, groups=["d"])]
        train, valid = temporal_split(rows, 0, 100, 200, 5)
        self.assertEqual([r["hash"] for r in train], ["d"])
        self.assertEqual([r["hash"] for r in valid], ["c"])
        delay_train, _ = temporal_split(rows, 0, 100, 200, 5, target="delay")
        self.assertEqual([r["hash"] for r in delay_train], ["b", "d"])

    def test_go_byte_array_hashes_and_zero_send(self):
        records = [{"type": "reactive_intent", "mode": "live", "transaction": [0]*32,
                    "intent": {"ID": [1]*32}, "submission": None},
                   {"type": "reactive_intent", "mode": "live", "transaction": [2]*32,
                    "intent": {"ID": [1]*32}, "submission": {"StartedAtNS": 1000000000}}]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"journal.jsonl"
            path.write_bytes(b"".join(encoded(r) for r in records))
            rows, summary = build_dataset([path], 1500)
        self.assertEqual(summary["submitted"], 1)
        self.assertEqual(summary["counts"]["missing_decision_features"], 1)
        self.assertEqual(rows, [])

    def test_dataset_canonical_void_and_unresolved(self):
        features = [0.] * len(FEATURES)
        intent = {"ID": "intent", "Chain": {"ChainID": 10}, "Version": {"Anchor": {"Hash": "anchor"}},
                  "Route": {"Steps": [{"Pool": {"Kind": 2}, "PriceBound": 1}]},
                  "Call": {"TipCap": 10, "GasLimit": 100000, "CodeHash": SCOPE["code_hash"]},
                  "Conversion": {"Numerator": 1, "Denominator": 1},
                  "Bid": {"ModelTip": 10, "DecisionNS": 1000000000, "WinRaw": .5,
                          "Learning": {"schema": SCHEMA, "features": features, "baseline_win": .5}}}
        def send(tx):
            return {"type": "reactive_intent", "mode": "live", "transaction": tx, "intent": intent,
                    "submission": {"StartedAtNS": 1050000000}}
        def outcome(tx, known, success=False):
            return {"type": "settlement", "observed_at_ns": known*1000000,
                    "receipt": {"Hash": tx, "IntentID": "intent", "Basis": "canonical_receipt", "Success": success,
                                "Block": {"Hash": "block"}, "Profit": 100,
                                "Fees": {"gas_used": 40000, "total_fee": 10}}}
        records = [send("win"), send("void"), send("pending"), send("legacy"),
                   outcome("win", 1200, True), outcome("void", 1200), outcome("legacy", 0),
                   outcome("win", 1400, True),
                   {"type": "settlement_voided", "transaction": "win", "observed_at_ns": 1399999999},
                   {"type": "settlement_voided", "transaction": "void", "observed_at_ns": 1300000000}]
        keys = {name: [i]*32 for i, name in enumerate(["win", "void", "pending", "legacy"], 2)}
        intent["ID"] = [1]*32
        intent["Call"]["CodeHash"] = [17]*32
        for record in records:
            if "transaction" in record:
                record["transaction"] = keys[record["transaction"]]
            if "receipt" in record:
                record["receipt"]["Hash"] = keys[record["receipt"]["Hash"]]
                record["receipt"]["IntentID"] = [1]*32
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"journal.jsonl"
            path.write_bytes(b"".join(encoded(r) for r in records) + b'{"unfinished":')
            rows, summary = build_dataset([path], 1500)
        by_hash = {name: next(r for r in rows if r["hash"] == "0x"+bytes(key).hex()) for name, key in keys.items()}
        self.assertEqual(by_hash["win"]["scope"], SCOPE)
        self.assertEqual(by_hash["win"]["success"], 1)
        self.assertEqual(by_hash["win"]["known_ms"], 1400)
        self.assertEqual(by_hash["win"]["net_wei"], 90)
        self.assertIsNone(by_hash["void"]["success"])
        self.assertIsNone(by_hash["pending"]["success"])
        self.assertIsNone(by_hash["legacy"]["success"])
        self.assertEqual(summary["counts"]["legacy_receipt_without_availability_time"], 1)
        self.assertEqual(by_hash["win"]["delay_ms"], 50)


class TrainingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows, cls.split, cls.end, cls.now = examples()
        cls.artifact, cls.report = train_candidate(cls.rows, start_ms=cls.split-2000000,
                                                  split_ms=cls.split, end_ms=cls.end, created_ms=cls.now)

    def test_portable_training_and_go_parity(self):
        self.assertIsNotNone(self.artifact)
        self.assertEqual(self.artifact["features"], FEATURES)
        self.assertLess(self.report["scores"]["candidate"]["brier"], .1)
        validator = Path(__file__).resolve().parents[2]/"bin/bidmodel"
        self.assertTrue(validator.exists(), "build cmd/bidmodel before Python tests")
        vectors = [r["features"] for r in self.rows[::20]] + [self.artifact["low"], self.artifact["high"]]
        with tempfile.TemporaryDirectory() as directory:
            digest = archive(directory, self.artifact, self.report, validator, vectors)
            payload = (Path(directory)/"releases"/(digest+".json")).read_bytes()
            self.assertEqual(json.loads(payload)["approved"], self.report["approved"])
            self.assertFalse((Path(directory)/"active.json").exists())
            unapproved = copy.deepcopy(self.artifact)
            unapproved["approved"] = False
            with self.assertRaisesRegex(ValueError, "did not pass"):
                archive(directory, unapproved, {**self.report, "approved": False}, validator, vectors, promote=True)
            self.assertFalse((Path(directory)/"active.json").exists())
            self.assertTrue(self.artifact["approved"])
            archive(directory, self.artifact, self.report, validator, vectors, promote=True)
            self.assertEqual((Path(directory)/"active.json").read_bytes(), payload)
            rollback(directory, digest, validator)
            self.assertEqual((Path(directory)/"active.json").read_bytes(), payload)
            release = Path(directory)/"releases"/(digest+".json")
            release.write_bytes(payload + b" ")
            with self.assertRaisesRegex(ValueError, "checksum"):
                rollback(directory, digest, validator)
            self.assertEqual((Path(directory)/"active.json").read_bytes(), payload)

    def test_controller_cycle_promotes_once_from_journal(self):
        root = Path(__file__).resolve().parents[2]
        now = time.time_ns()//1000000
        end = (now-10000)//1800000*1800000
        split = end-1800000
        records = []
        for i, row in enumerate(self.rows):
            at = split-3600000+i*2000 if i < 800 else split+(i-800)*4000
            tx = list((i+1).to_bytes(32, "big"))
            tip = 100+i%20
            intent = {"ID": tx, "Chain": {"ChainID": 10},
                      "Version": {"Anchor": {"Hash": list((i+1).to_bytes(32, "big"))}},
                      "Route": {"Steps": [{"Pool": {"Kind": 2}, "PriceBound": 1}]},
                      "Call": {"TipCap": tip, "GasLimit": 1000000, "CodeHash": [17]*32},
                      "Conversion": {"Numerator": 1, "Denominator": 1},
                      "Bid": {"ModelTip": tip, "DecisionNS": at*1000000, "WinRaw": .5,
                              "Learning": {"schema": SCHEMA, "features": row["features"], "baseline_win": .5}}}
            records.append({"type": "reactive_intent", "mode": "live", "transaction": tx, "intent": intent,
                            "submission": {"StartedAtNS": int((at+row["delay_ms"])*1000000)}})
            records.append({"type": "settlement", "observed_at_ns": (at+100)*1000000,
                            "receipt": {"Hash": tx, "IntentID": tx, "Basis": "canonical_receipt", "Success": bool(row["success"]),
                                        "Block": {"Hash": tx}, "Profit": 1100,
                                        "Fees": {"gas_used": int(row["gas_ratio"]*1000000), "total_fee": 1000}}})
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory)/"journal.jsonl"
            journal.write_bytes(b"".join(encoded(r) for r in records))
            registry = Path(directory)/"models"
            command = [sys.executable, str(root/"scripts/bid-learning/main.py"), "cycle", "--input", str(journal),
                       "--directory", str(registry), "--validator", str(root/"bin/bidmodel"), "--promote"]
            first = subprocess.run(command, check=True, capture_output=True, text=True)
            report = json.loads(first.stdout)
            self.assertTrue(report["promoted"], report.get("reasons"))
            self.assertTrue((registry/"active.json").exists())
            second = subprocess.run(command, check=True, capture_output=True, text=True)
            self.assertEqual(json.loads(second.stdout)["status"], "already_evaluated")

    def test_future_labels_do_not_change_fit(self):
        changed = copy.deepcopy(self.rows)
        for row in changed:
            if row["decision_ms"] >= self.split:
                row["success"] = 1-row["success"]
        artifact, _ = train_candidate(changed, start_ms=self.split-2000000,
                                     split_ms=self.split, end_ms=self.end, created_ms=self.now)
        for head in ["win", "revert_gas", "delay"]:
            self.assertEqual(artifact[head], self.artifact[head])

    def test_small_window_cannot_publish(self):
        artifact, report = train_candidate(self.rows[:41], start_ms=self.split-2000000,
                                           split_ms=self.split, end_ms=self.end, created_ms=self.now)
        self.assertIsNone(artifact)
        self.assertFalse(report["approved"])

    def test_out_of_support_and_nan(self):
        f = self.rows[0]["features"].copy()
        f[1] = self.artifact["high"][1]+1
        self.assertIsNone(predict(self.artifact, f))
        f[1] = math.nan
        with self.assertRaises(ValueError):
            predict(self.artifact, f)


if __name__ == "__main__":
    unittest.main()
