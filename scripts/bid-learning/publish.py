"""Atomic releases are local files; this module never deploys or submits trades."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from schema import predict


def atomic_write(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=".pending-")
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode() + b"\n"


def validate_go(validator, path, artifact, vectors):
    scope = artifact["scope"]
    command = [str(Path(validator).resolve()), "-model", str(path), "-chain", str(scope["chain_id"]),
               "-code-hash", scope["code_hash"]]
    if scope["precheck"]:
        command.append("-precheck")
    result = subprocess.run(command, check=True, capture_output=True, text=True, timeout=30)
    identity = json.loads(result.stdout)
    result = subprocess.run(command + ["-predict"], input="".join(json.dumps(f)+"\n" for f in vectors),
                            check=True, capture_output=True, text=True, timeout=30)
    predictions = [json.loads(line) for line in result.stdout.splitlines()]
    if len(predictions) != len(vectors):
        raise ValueError("Go inference parity result count mismatch")
    for f, actual in zip(vectors, predictions):
        expected = predict(artifact, f)
        if actual["supported"] != (expected is not None):
            raise ValueError("Go support decision differs")
        if expected and any(abs(actual[k]-v) > 1e-10 * max(1., abs(v)) for k, v in expected.items()):
            raise ValueError("Go inference differs from exported model")
    return identity


def archive(directory, artifact, report, validator, vectors, promote=False):
    directory = Path(directory)
    payload = encoded(artifact)
    digest = hashlib.sha256(payload).hexdigest()
    path = directory / "releases" / (digest + ".json")
    atomic_write(path, payload)
    checked = validate_go(validator, path, artifact, vectors)
    if checked["sha256"] != digest:
        raise ValueError("validated artifact digest differs")
    report = {**report, "artifact_sha256": digest, "go_parity_vectors": len(vectors)}
    atomic_write(directory / "releases" / (digest + ".report.json"), encoded(report))
    if promote:
        if not artifact["approved"] or not report["approved"]:
            raise ValueError("candidate did not pass validation")
        atomic_write(directory / "active.json", payload)
    return digest


def rollback(directory, digest, validator):
    if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
        raise ValueError("rollback requires a SHA-256 release ID")
    directory = Path(directory)
    path = directory / "releases" / (digest + ".json")
    payload = path.read_bytes()
    if hashlib.sha256(payload).hexdigest() != digest:
        raise ValueError("release checksum mismatch")
    artifact = json.loads(payload)
    report = json.loads((directory / "releases" / (digest + ".report.json")).read_bytes())
    if not artifact["approved"] or not report["approved"] or report["artifact_sha256"] != digest:
        raise ValueError("cannot activate an unapproved release")
    if not artifact["created_ms"] <= time.time_ns()//1000000 < artifact["expires_ms"]:
        raise ValueError("cannot activate an expired release")
    validate_go(validator, path, artifact, [artifact["low"], artifact["high"]])
    atomic_write(directory / "active.json", payload)
