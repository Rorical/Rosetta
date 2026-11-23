# 23. Configuration through flags and files, environment only for secrets

## Status

Accepted

## Context

An instance's behaviour depends on dozens of parameters: network, endpoints, search limits, discovery scope, reactive limits, bidding mode. When some of these come from flags, some from files and some from environment variables, the effective configuration of a running process is hard to reconstruct, and a stray variable in a service unit can silently change trading.

Some values must not appear in a command line, which other users can see in the process list, or in the repository: private keys, API keys embedded in endpoint URLs, the console token.

## Decision

- Runtime parameters come from command-line flags defined in `internal/config/flags.go`. Structured settings live in JSON files named by flags: the network profile (`-network-file`), search (`-search-config`), discovery (`-discovery-config`), reactive policy (`-reactive-config`), execution and paper configs, pool lists and catalogs.
- JSON files are decoded strictly. Unknown fields and trailing data are errors, so a typo fails startup instead of being ignored.
- Environment variables carry only secrets and endpoints that embed a key: the signing secret reference (`ROSETTA_SIGNING_SECRET_ID`) and keystore passphrase, the Fast Feed key, keyed RPC endpoints for submission, tracing, history logs and discovery state, and the console token.
- An endpoint may be given by flag when it carries no key (for example `-submit-rpc`); the environment variable is the fallback for keyed URLs.
- Every new flag or environment variable is documented in `docs/configuration.md`.
- `configs/` holds only publishable pool catalogs and strategy configs. Reactive policies are committed only as `*.example.json` templates; real ones, with addresses, limits and journal paths, stay outside the repository.
- A few process-level diagnostics still read the environment (`ROSETTA_LOG_LEVEL`, `ROSETTA_WATCHDOG`, `ROSETTA_FEED_ARRIVALS_OUT`). They do not affect trading decisions; new trading parameters must not follow that pattern.

## Consequences

- A process's trading configuration is its command line plus the files it names, which makes a deployment reproducible from its unit file and config directory.
- Secrets never appear in `ps` output, in committed files or in flag defaults.
- Adding a parameter means a flag or a config field, a validation rule and a line in `docs/configuration.md`.
