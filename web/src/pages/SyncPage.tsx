import { useState } from "react";
import type { SourceView } from "../api";
import { Empty, Group, Row, Status } from "../components/bits";
import { HistoryChart } from "../components/HistoryChart";
import { RangePicker } from "../components/RangePicker";
import { formatBytes, formatCompact, formatCount, formatRate, formatSeconds, formatTime } from "../format";
import { useLive } from "../live";

const formatPercent = (fraction: number) => `${(fraction * 100).toFixed(0)}%`;
const formatEther = (value: number) => `${Number(value.toPrecision(4))}`;
const feedTitles: Record<string, string> = { fast: "Flashblocks", standard: "Standard" };

function Feed({ name, source }: { name: string; source: SourceView }) {
  const timestamp = source.gauges.head_timestamp_seconds;
  const receiving = (source.counters.messages ?? 0) > 0;
  return (
    <Group title={feedTitles[name] ?? name}>
      <Row label="Connection">
        {source.gauges.feed_connected ? <Status tone="good">Connected</Status> : <Status tone={receiving ? "serious" : "neutral"}>{receiving ? "Disconnected" : "Unused"}</Status>}
      </Row>
      <Row label="Messages">{receiving ? `${formatRate(source.rates.messages)}/s` : "—"}</Row>
      <Row label="Head block">{formatCount(source.gauges.head_block || undefined)}</Row>
      <Row label="Head age">{timestamp ? formatSeconds(Date.now() / 1000 - timestamp) : "—"}</Row>
      <Row label="Blocks behind">{formatCount(source.gauges.sync_lag_blocks)}</Row>
      <Row label="Frame backlog">{formatCount(source.gauges.feed_backlog)}</Row>
      <Row label="Disconnects / gap recoveries">{formatCount(source.counters.feed_disconnects)} / {formatCount(source.counters.feed_recoveries)}</Row>
      <Row label="Stream invalidations">{formatCount(source.counters.stream_invalidations)}</Row>
    </Group>
  );
}

export function SyncPage({ prometheus }: { prometheus: boolean }) {
  const { selected } = useLive();
  const [range, setRange] = useState("1h");
  if (!selected?.sources?.standard) return <Empty>No data from this instance yet.</Empty>;
  const standard = selected.sources.standard;
  const budget = standard.gauges.rpc_call_budget ?? 0;
  const calls = standard.counters.rpc_method_calls ?? 0;
  const installed = standard.gauges.graph_installed_seconds;
  return (
    <>
      <header className="page-title">
        <h1>Sync and nodes</h1>
      </header>
      <div className="columns">
        {Object.entries(selected.sources).sort(([a], [b]) => a.localeCompare(b)).map(([name, source]) => <Feed key={name} name={name} source={source} />)}
        <Group title="Discovery">
          <Row label="Tracked pools">{formatCount(standard.gauges.pools || undefined)}</Row>
          <Row label="Graph version">{formatCount(standard.gauges.graph_version || undefined)}</Row>
          <Row label="Last install">{installed ? `${formatSeconds(Date.now() / 1000 - installed)} ago` : "—"}</Row>
          <Row label="Cycles">{formatCount(standard.gauges.graph_cycles || undefined)}{standard.gauges.graph_truncated ? " (truncated)" : ""}</Row>
          <Row label="Completed / failed">{formatCount(standard.counters.discovery_completed)} / {formatCount(standard.counters.discovery_errors)}</Row>
          <Row label="Pools added / skipped">{formatCount(standard.counters.discovery_pools_added)} / {formatCount(standard.counters.discovery_pools_skipped)}</Row>
        </Group>
        <Group title="RPC">
          <Row label="Method calls">{budget > 0 ? `${formatCount(calls)} / ${formatCount(budget)}` : formatCount(calls)}</Row>
          <Row label="HTTP requests">{formatCount(standard.counters.rpc_http_attempts)}</Row>
          <Row label="Retries">{formatCount(standard.counters.rpc_retries)}</Row>
          <Row label="Failures">{formatCount(standard.counters.rpc_http_errors)}</Row>
          <Row label="Throttled">{formatCount(standard.counters.rpc_throttled)}</Row>
        </Group>
        <Group title="Process">
          <Row label="Version"><code className="hex">{selected.status?.version ?? "—"}</code></Row>
          <Row label="Started">{formatTime(selected.status?.started_at_ms ?? 0)}</Row>
          <Row label="CPU">{selected.online ? `${selected.cpu_percent.toFixed(0)}%` : "—"}</Row>
          <Row label="Memory">{selected.online ? formatBytes(selected.process.memory_bytes) : "—"}</Row>
          <Row label="Goroutines">{formatCount(selected.process.goroutines)}</Row>
          <Row label="Open files">{formatCount(selected.process.open_files)}</Row>
          {standard.gauges.disk_free_bytes ? <Row label="Disk free">{formatBytes(standard.gauges.disk_free_bytes)}</Row> : null}
        </Group>
      </div>
      <Group title="Trends" trailing={<RangePicker value={range} onChange={setRange} />}>
        <div className="charts">
          <HistoryChart instance={selected.name} chart="messages" range={range} title="Messages (per second)" format={formatRate} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="head_age" range={range} title="Head age" format={formatSeconds} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="sync_lag" range={range} title="Blocks behind" format={formatCompact} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="pools" range={range} title="Tracked pools" format={formatCompact} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="rpc_failures" range={range} title="RPC failures and throttles (per second)" format={formatRate} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="cpu" range={range} title="CPU" format={formatPercent} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="memory" range={range} title="Memory" format={formatBytes} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="wallet" range={range} title="Wallet balance (ETH)" format={formatEther} prometheus={prometheus} />
        </div>
      </Group>
    </>
  );
}
