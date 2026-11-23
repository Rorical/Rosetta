#!/usr/bin/env python3
"""Collect, train, evaluate and atomically publish an experimental bid model."""
import argparse
import fcntl
import glob
import hashlib
import json
from pathlib import Path
import time

from dataset import build_dataset
from publish import archive, atomic_write, encoded, rollback


def cycle(args):
    from train import train_candidate
    now = time.time_ns() // 1000000
    # Adjacent, fixed UTC slots prevent repeatedly selecting a model against
    # overlapping validation observations. Labels must already be available.
    width = args.validation_minutes * 60000
    end = (now - args.settle_seconds*1000) // width * width
    split = end-width
    directory = Path(args.directory)
    done = directory / "cycles" / f"{end}.json"
    if done.exists():
        return {"status": "already_evaluated", "validation_until_ms": end}
    paths = sorted({name for pattern in args.input for name in glob.glob(pattern)})
    if not paths:
        raise ValueError("no input journals matched")
    rows, summary = build_dataset(paths, end, args.max_records)
    # One scope per controller. A contract upgrade gets a new registry.
    scopes = {json.dumps(r["scope"], sort_keys=True) for r in rows}
    if len(scopes) > 1:
        raise ValueError("mixed execution scopes; provide scope-specific journals")
    active = directory / "active.json"
    incumbent = json.loads(active.read_bytes()) if active.exists() else None
    artifact, report = train_candidate(rows, start_ms=split-args.train_hours*3600000,
                                       split_ms=split, end_ms=end, created_ms=now,
                                       gap_ms=args.gap_seconds*1000, min_train=args.min_train,
                                       min_valid=args.min_valid, ttl_ms=args.ttl_minutes*60000,
                                       half_life_ms=args.half_life_minutes*60000, incumbent=incumbent)
    report["dataset"] = summary
    if artifact:
        # Validate Go/Python parity on real training and held-out contexts,
        # including both action boundaries. No signing/network capability.
        vectors = [r["features"] for r in rows[-256:]] + [artifact["low"], artifact["high"]]
        digest = archive(directory, artifact, report, args.validator, vectors,
                         promote=args.promote and artifact["approved"])
        report.update(artifact_sha256=digest, promoted=args.promote and artifact["approved"])
    atomic_write(done, encoded(report))
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    export = commands.add_parser("export")
    export.add_argument("--input", action="append", required=True)
    export.add_argument("--output", required=True)
    export.add_argument("--as-of-ms", type=int, default=None)
    export.add_argument("--max-records", type=int, default=1000000)
    for name in ["cycle", "watch"]:
        command = commands.add_parser(name)
        command.add_argument("--input", action="append", required=True, help="journal glob, repeatable")
        command.add_argument("--directory", required=True)
        command.add_argument("--validator", required=True, help="built cmd/bidmodel executable")
        command.add_argument("--promote", action="store_true", help="activate only accepted models")
        command.add_argument("--train-hours", type=int, default=6)
        command.add_argument("--validation-minutes", type=int, default=30)
        command.add_argument("--gap-seconds", type=int, default=5)
        command.add_argument("--settle-seconds", type=int, default=10)
        command.add_argument("--ttl-minutes", type=int, default=60)
        command.add_argument("--half-life-minutes", type=int, default=60)
        command.add_argument("--min-train", type=int, default=500)
        command.add_argument("--min-valid", type=int, default=200)
        command.add_argument("--max-records", type=int, default=1000000)
        command.add_argument("--poll-seconds", type=int, default=60)
    audit = commands.add_parser("evaluate")
    audit.add_argument("--input", action="append", required=True)
    audit.add_argument("--model", required=True)
    audit.add_argument("--since-ms", type=int, required=True)
    audit.add_argument("--until-ms", type=int, required=True)
    audit.add_argument("--output", required=True)
    audit.add_argument("--max-records", type=int, default=1000000)
    back = commands.add_parser("rollback")
    back.add_argument("--directory", required=True)
    back.add_argument("--release", required=True)
    back.add_argument("--validator", required=True)
    args = parser.parse_args()
    if hasattr(args, "max_records") and args.max_records <= 0:
        parser.error("max-records must be positive")
    if args.command == "evaluate":
        from evaluate import evaluate
        artifact_bytes = Path(args.model).read_bytes()
        artifact = json.loads(artifact_bytes)
        now = time.time_ns()//1000000
        if not artifact["created_ms"] <= args.since_ms < args.until_ms <= min(now, artifact["expires_ms"]):
            parser.error("evaluation must follow model creation and stay within its lifetime")
        paths = sorted({name for pattern in args.input for name in glob.glob(pattern)})
        if not paths:
            parser.error("no input journals matched")
        rows, summary = build_dataset(paths, now, args.max_records)
        selected = [r for r in rows if args.since_ms <= r["decision_ms"] < args.until_ms and r["success"] is not None]
        if not selected or any(r["scope"] != artifact["scope"] for r in selected):
            parser.error("no scoreable rows or mismatched execution scope")
        report = evaluate(artifact, selected)
        report.update(artifact_sha256=hashlib.sha256(artifact_bytes).hexdigest(), dataset=summary, independent_window={"since_ms": args.since_ms, "until_ms": args.until_ms})
        atomic_write(args.output, encoded(report))
        print(json.dumps(report, allow_nan=False))
        return
    if args.command == "export":
        paths = sorted({name for pattern in args.input for name in glob.glob(pattern)})
        if not paths:
            parser.error("no input journals matched")
        rows, summary = build_dataset(paths, args.as_of_ms or time.time_ns()//1000000, args.max_records)
        atomic_write(args.output, b"".join(encoded(row) for row in rows))
        print(json.dumps(summary))
        return
    if args.command in ["cycle", "watch"]:
        for key in ["train_hours", "validation_minutes", "gap_seconds", "settle_seconds", "ttl_minutes", "half_life_minutes", "min_train", "min_valid", "poll_seconds"]:
            if getattr(args, key) <= 0:
                parser.error(key + " must be positive")
        if args.ttl_minutes > 1440:
            parser.error("ttl-minutes cannot exceed 1440")
    directory = Path(args.directory)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (directory / ".controller.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.command == "rollback":
            rollback(directory, args.release, args.validator)
            print(json.dumps({"activated": args.release}))
            return
        while True:
            result = cycle(args)
            print(json.dumps(result, allow_nan=False), flush=True)
            if args.command != "watch":
                return
            time.sleep(args.poll_seconds)


if __name__ == "__main__":
    main()
