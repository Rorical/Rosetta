# Security policy

Rosetta signs and sends transactions that move real funds, and its executor contract holds balances between trades. Treat any bug that can lose funds, leak a key, or let a third party drive the executor as a security issue.

## Reporting a vulnerability

Report privately through the **Report a vulnerability** form on this repository's Security tab. Do not open a public issue, pull request, or discussion for a suspected vulnerability.

Include what you can of:

- the affected component (`contracts/`, `internal/exec`, signing, the control socket, the console)
- the commit you tested
- steps or a proof of concept, ideally a Foundry test or a Go test
- the impact you expect (funds at risk, key exposure, denial of service)

Expect an acknowledgement within 72 hours. A fix and advisory are coordinated with the reporter before disclosure.

## Scope

In scope:

- `contracts/src`: anything that lets a caller other than the owner or an authorized operator move funds, bypass the final profit check, or spoof a pool callback.
- `internal/exec`: nonce handling, fee and principal limits, the stop file, and anything that could send a transaction outside the configured limits.
- Key handling: signer loading from AWS Secrets Manager or `keystore://`, and any path that could write or log key material.
- `internal/control` and `internal/console`: anything that lets an unauthenticated client change delivery gates.

Out of scope:

- Losing trades, reverted transactions, or unprofitable bids. These are economic outcomes the limits are designed to cap.
- Issues that require control of the operator's host, RPC endpoint, or AWS account.
- Third-party protocols the executor interacts with.

## Running it safely

- Keep only bounded funds in hot wallets and the executor.
- Always configure `max_transaction_fee`, `max_daily_fee`, `assets[].max_input`, and `stop_file` before enabling live sending. See [docs/configuration.md](docs/configuration.md).
- Never commit keys, keystores, `.env` files, or RPC and feed URLs that embed credentials.
