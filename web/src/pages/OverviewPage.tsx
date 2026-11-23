import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type InstanceView } from "../api";
import { DispatcherGroup, Funnel, dispatchState } from "../components/Dispatcher";
import { Empty, Group, Row, Status, type Tone } from "../components/bits";
import { HistoryChart } from "../components/HistoryChart";
import { RangePicker } from "../components/RangePicker";
import { SendsTable } from "../components/SendsTable";
import { formatCount, formatDuration, formatEth, formatRate, formatSeconds } from "../format";
import { errorEvents } from "../labels";
import { useLive } from "../live";
import { gauge, stage, utcDay } from "../metrics";
import { useUnits } from "../units";

const formatSecondsValue = (seconds: number) => formatDuration(seconds * 1e9);

function health(instance: InstanceView, errors: number, sending: { tone: Tone; label: string }): { tone: Tone; label: string } {
  if (!instance.online) return { tone: "critical", label: "Offline" };
  if (sending.tone === "critical") return sending;
  if (errors > 0) return { tone: "serious", label: "Errors" };
  return { tone: "good", label: "Healthy" };
}

function Quantile({ instance, name }: { instance: InstanceView; name: string }) {
  const latency = stage(instance, name);
  if (!latency || latency.count === 0) return <>—</>;
  return <>{formatDuration(latency.p50_ns)} / {latency.overflow ? "> " : ""}{formatDuration(latency.p90_ns)}</>;
}

export function OverviewPage({ prometheus }: { prometheus: boolean }) {
  const { selected } = useLive();
  const [range, setRange] = useState("24h");
  const name = selected?.name ?? "";
  const started = selected?.status?.started_at_ms ?? 0;
  const alerts = useQuery({ queryKey: ["alerts"], queryFn: api.alerts, refetchInterval: 15_000, enabled: prometheus });
  const execution = useQuery({ queryKey: ["execution", name, started], queryFn: () => api.execution(name, started), enabled: Boolean(selected), refetchInterval: 5_000 });
  const sends = useQuery({ queryKey: ["sends", name, "", 5], queryFn: () => api.sends(name, "", 5), enabled: Boolean(selected), refetchInterval: 3_000 });
  const pnl = useQuery({ queryKey: ["pnl", name], queryFn: () => api.pnl(name), enabled: Boolean(selected), refetchInterval: 10_000 });
  const units = useUnits(selected?.name);
  if (!selected) return <Empty>Connecting to instance.</Empty>;

  const dispatch = execution.data?.dispatch;
  const sending = dispatchState(selected, dispatch);
  const errors = Object.values(selected.sources ?? {}).reduce((sum, source) => sum + errorEvents.reduce((total, event) => total + (source.rates[event] ?? 0), 0), 0);
  const state = health(selected, errors, sending);
  const firing = (alerts.data ?? []).filter((alert) => alert.state === "firing" && alert.instance === selected.name);
  const today = (pnl.data ?? []).filter((row) => row.day === utcDay(Date.now()));
  const sum = (pick: (row: (typeof today)[number]) => number) => today.reduce((total, row) => total + pick(row), 0);
  const fast = selected.sources?.fast;
  const lag = gauge(selected, "sync_lag_blocks");
  const headTimestamp =gauge(selected, "head_timestamp_seconds", "fast") || gauge(selected, "head_timestamp_seconds");

  return (
    <>
      <header className="page-title">
        <h1>{selected.status?.network ?? selected.name}</h1>
        <Status tone={state.tone}>{state.label}</Status>
      </header>
      {!selected.online && selected.error && <p className="banner critical" role="alert">{selected.error === "control socket unreachable" ? "Cannot reach the instance control socket." : selected.error}</p>}
      {dispatch?.halted_reason && <p className="banner critical" role="alert">Dispatcher halted: {dispatch.halted_reason}</p>}
      {firing.map((alert) => (
        <p key={`${alert.name}-${alert.active_at}`} className={`banner ${alert.severity === "critical" ? "critical" : "warning"}`} role="alert">
          {alert.summary || alert.name}
        </p>
      ))}
      {selected.sources && <Funnel instance={selected} />}
      <div className="columns">
        <Group title="Today (UTC)">
          <Row label="Net ≈">{today.length ? <Status tone={sum((row) => row.net_native) < 0 ? "critical" : "good"}>{formatEth(sum((row) => row.net_native))}</Status> : "—"}</Row>
          <Row label="Gross ≈">{today.length ? formatEth(sum((row) => row.profit_native)) : "—"}</Row>
          <Row label="Fees">{today.length ? formatEth(sum((row) => row.fee)) : "—"}</Row>
          <Row label="Success / reverted">{today.length ? `${formatCount(sum((row) => row.succeeded))} / ${formatCount(sum((row) => row.reverted))}` : "—"}</Row>
        </Group>
        <DispatcherGroup instance={selected} dispatch={dispatch} />
        <Group title="Data and latency">
          <Row label="Flashblocks">{fast?.gauges.feed_connected ? <Status tone="good">Connected</Status> : <Status tone={(fast?.counters.messages ?? 0) > 0 ? "serious" : "neutral"}>{(fast?.counters.messages ?? 0) > 0 ? "Disconnected" : "Unused"}</Status>}</Row>
          <Row label="Frame rate">{fast ? `${formatRate(fast.rates.messages)}/s` : "—"}</Row>
          <Row label="Head age">{headTimestamp ? formatSeconds(Date.now() / 1000 - headTimestamp) : "—"}</Row>
          <Row label="Canonical lag">{lag === undefined ? "—" : `${formatCount(lag)} ${lag === 1 ? "block" : "blocks"}`}</Row>
          <Row label="Search compute p50 / p90"><Quantile instance={selected} name="search_evaluate" /></Row>
          <Row label="Trigger to send p50 / p90"><Quantile instance={selected} name="reactive_trigger_to_send" /></Row>
        </Group>
      </div>
      <Group title="Recent transactions" trailing={<Link className="more-link" to="/sends">All transactions</Link>}>
        {sends.data?.length ? <SendsTable sends={sends.data} units={units} compact /> : <Empty>{sends.isPending ? "" : "No transactions sent yet."}</Empty>}
      </Group>
      <Group title="Trends" trailing={<RangePicker value={range} onChange={setRange} />}>
        <div className="charts">
          <HistoryChart instance={selected.name} chart="sends" range={range} title="Sends and outcomes (per hour)" format={formatRate} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="trigger_to_send" range={range} title="Trigger to send" format={formatSecondsValue} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="search_evaluate" range={range} title="Search compute" format={formatSecondsValue} prometheus={prometheus} />
          <HistoryChart instance={selected.name} chart="sync_lag" range={range} title="Blocks behind" format={formatRate} prometheus={prometheus} />
        </div>
      </Group>
    </>
  );
}
