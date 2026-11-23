# 21. Signing only in `internal/exec`, with keys from a secret manager or an explicit keystore

## Status

Accepted

## Context

The engine has many components that could plausibly want to send a transaction: the reactive planner, offline audit tools, probes, the console. Each additional place that can sign is another place a bug or a compromised dependency can spend funds, and another code path that bypasses the dispatcher's fee caps, nonce journal and stop file.

Private keys on disk in plain text are a standing risk on any host. Cloud secret managers keep the key off disk and let access be granted to one service identity, but a single host without a cloud identity cannot use them.

## Decision

- `internal/exec` is the only package that signs or sends transactions. Signing happens inside the dispatcher, after the gate, limits and token and pool checks pass, and the dispatcher verifies the signed transaction against the unsigned envelope it approved.
- Tools under `cmd/` other than `cmd/rosetta` do not sign or send. The console never imports `internal/exec` (ADR 0022).
- The signing key is referenced, never configured inline. `ROSETTA_SIGNING_SECRET_ID` (and each lane's `secret_id`) selects the source:
  - an AWS Secrets Manager secret ID, read once through the default credential chain;
  - `gcp://projects/<number>/secrets/<name>/versions/<number>`, a Google Secret Manager secret pinned to a numeric version so a later rotation cannot change the key unnoticed;
  - `keystore://<path>`, a local go-ethereum V3 key file, decrypted once with a passphrase from `ROSETTA_KEYSTORE_PASSWORD_FILE` or `ROSETTA_KEYSTORE_PASSWORD`. This is an explicit opt-in for single hosts without a cloud identity.
- Whatever the source, the derived address must equal the configured sender or startup fails. The key is held only in process memory, and decrypted material and passphrases are cleared after use.
- Error messages name the failed step and never include key or passphrase material.

## Consequences

- Auditing what can move funds means reading one package.
- A compromised host can still sign while the process runs, since the key is in memory. The hot wallet holds only what the live limits can spend (ADR 0018), and profit accumulates in the executor, where only the owner can sweep it. Keeping the owner key off the trading host removes that profit from a host compromise, at the cost of the automatic native refill, which sweeps to the owner as the sender.
- The keystore option has no hardware isolation, remote revocation or decryption audit trail; multi-host or larger deployments should use a secret manager.
- Rotating a key means a new secret version or file, an operator registration on the executor, and a restart.
