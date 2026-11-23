# 22. Operator console holds no keys and gates only at delivery

## Status

Accepted

## Context

With one process per chain (ADR 0001), an operator needs one place to see every instance and to stop trading quickly. Putting that view inside the trading process would tie a dashboard's crashes, releases and frontend dependencies to the process that holds the hot key. A dashboard with the power to sign, change limits or edit state would also become a second path to the funds.

Pausing has to be safe in both directions: a pause must take effect even when the disk is failing, and a resume must never happen unless it was recorded.

## Decision

- `cmd/rosetta-console` is a separate binary (Go backend with an embedded TypeScript frontend). It holds no keys, never imports `internal/exec`, and reaches an instance only through that instance's control socket.
- Each trading process with `-control-dir` opens a private directory (mode 0700, one instance per directory, held by a lock) containing a Unix socket and `gate.json`. Access to the socket is the authorization boundary.
- The gate decides only whether a result leaves the process: it blocks delivery of ready plans and, in reactive mode, new live sends, replacements and maintenance transactions at the last check before signing. Cancellations still go out, since they only reduce exposure. The gate is read outside state transitions and never changes search or state.
- A pause takes effect in memory even if writing `gate.json` fails. A resume takes effect only after the write succeeds. A corrupt gate file refuses startup. Every change is written to the audit log before it is applied.
- The stop file (ADR 0018) remains a separate, harder stop that includes cancellations.
- Data ownership is split: Prometheus holds time series and alerts with fixed label sets; the control socket serves current state; the console's SQLite store holds per-opportunity detail and the audit log, ingested from the instances' existing JSONL output, so the trading hot path does no extra writes. Chart queries are fixed server-side; the browser cannot submit PromQL.
- The console listens on loopback only and requires an operator token (`ROSETTA_CONSOLE_TOKEN`). Sessions use HttpOnly, SameSite=Strict cookies; writes check Origin; Host is allowlisted against DNS rebinding; failed logins are rate-limited without locking out the correct token. Remote access goes through an SSH tunnel or an authenticating gateway.

## Consequences

- The console can crash or be redeployed without affecting trading, and the trading binary carries no frontend dependencies.
- A compromised console can pause or resume delivery and read operational data. It cannot sign, raise limits or alter state.
- Limits, send mode and bidding are changed in each instance's configuration; the console has no controls for them.
