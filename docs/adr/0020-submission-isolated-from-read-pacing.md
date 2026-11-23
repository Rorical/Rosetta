# 20. Submission isolated from read pacing

## Status

Accepted

## Context

Read traffic needs limits. Public and metered RPC endpoints rate-limit or bill per call, and a crash loop that re-bootstraps every pool, or a poll that rereads unchanged data, wastes quota quickly. The runtime therefore has a shared request policy: a per-run call budget (`-rpc-max-calls`), a minimum interval per method (`-rpc-min-interval`), cooldowns on rate-limit responses and a read cache.

Sends are a different kind of call. A signed transaction is worth something only for a few hundred milliseconds. When the submission client inherited the shared policy, a send queued behind a batch of reads and reached the sequencer hundreds of milliseconds late, which cost the race on every contested opportunity.

Global caps on useful reads have the same effect one step earlier: a per-minute cap on condition-word reads or route probes delayed the reads that make sends cheap or safe, and the losses from missing them cost more than the reads.

## Decision

- The submission client is isolated from the shared request policy. It ignores pacing, budgets and cooldowns, connects to the configured sequencer ingress (`-submit-rpc` or `ROSETTA_SUBMIT_RPC_URL`), checks the chain ID once and keeps the connection warm.
- Read limits target waste:
  - The read cache serves identical reads at the same immutable state once, and merges concurrent identical reads.
  - Bootstrap writes a pool snapshot as soon as it finishes, so a restart catches up from it instead of reloading every pool; a process far behind reseeds at the tip.
  - Per-key deduplication bounds repeated work: one condition-word read per pool state, one route probe and one revert diagnosis per route per minute, one gas measurement per route per ten minutes.
- There is no global per-minute cap on reads that serve a route being priced or sent.
- Rate-limited batch members are retried individually with growing cooldown, and only read-only members are retried.

## Consequences

- Send latency is the network round trip to the sequencer, independent of read load.
- RPC cost scales with useful work (pools changing, routes being priced) and is bounded by deduplication.
- An operator who needs a hard spending ceiling on a metered endpoint has to enforce it outside the process, for example with a watchdog on call counts that stops the service.
