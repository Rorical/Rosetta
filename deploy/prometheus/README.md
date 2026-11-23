# Prometheus

Scrape configuration and alert rules for `rosetta` and `rosetta-console`.

| File | Content |
|---|---|
| `prometheus.yml` | One `rosetta` target per instance (its `-metrics-listen` address) and the console's `/metrics` |
| `rules/rosetta.yml` | Alert rules for instances and the console |

Each `rosetta` target needs a `rosetta_instance` label equal to the name given to `rosetta-console -instance`, so the console can match history and alerts to an instance. Add one target per running instance. The example scrapes the bot on `127.0.0.1:9090`, so run Prometheus on another port and pass its URL to `rosetta-console -prometheus`.

```bash
promtool check config deploy/prometheus/prometheus.yml
prometheus --config.file=deploy/prometheus/prometheus.yml --web.listen-address=127.0.0.1:9091
```

| Alert | Fires when |
|---|---|
| `RosettaDown` | The instance does not answer scrapes for 1 minute. |
| `RosettaHeadStale` | The canonical head is more than 60 s old. |
| `RosettaSyncLagging` | The follower is more than 20 blocks behind the RPC tip for 2 minutes. |
| `RosettaFeedDisconnected` | A feed that carried messages in the last 15 minutes is disconnected. |
| `RosettaFeedFlapping` | A feed reconnected more than 3 times in 10 minutes. |
| `RosettaStateErrors` | Block mismatches, pool, checkpoint or reconcile errors occurred. |
| `RosettaQueueOverflow` | An input queue overflowed. |
| `RosettaRPCBudgetLow` | More than 80% of `-rpc-max-calls` is used. |
| `RosettaRPCThrottled` | The RPC endpoint rate-limited more than 5 calls in 5 minutes. |
| `RosettaDiscoveryFailing` | Pool discovery failed more than 3 times in 30 minutes. |
| `RosettaGraphTruncated` | The search graph hit its cycle limit. |
| `RosettaDiskLow` | Less than 50 GB of disk is free. |
| `RosettaDeliveryPaused` | The delivery gate has been paused for an hour. |
| `RosettaDispatchHalted` | The reactive dispatcher halted and sends nothing until an operator reconciles its nonce. |
| `RosettaDispatchStopped` | The reactive stop file has been present for an hour. |
| `RosettaConsoleInstanceOffline` | The console cannot reach an instance's control socket. |
| `RosettaConsoleTailStuck` | The console is more than 50 MB behind importing an instance's output. |

The `alerting` block points at an Alertmanager on `127.0.0.1:9093`; change it or remove it to match your setup. Metric families are listed in [configuration.md](../../docs/configuration.md#metrics-and-alerts).
