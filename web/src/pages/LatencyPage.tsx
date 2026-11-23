import { useState } from "react";
import type { SourceView } from "../api";
import { Empty, Group, Segmented } from "../components/bits";
import { HistoryChart } from "../components/HistoryChart";
import { RangePicker } from "../components/RangePicker";
import { formatCount, formatDuration, formatRate } from "../format";
import { eventGroups, sourceLabels, stageLabels, stageOrder } from "../labels";
import { useLive } from "../live";

const formatSecondsValue = (seconds: number) => formatDuration(seconds * 1e9);

const charts: [string, string][] = [
  ["search_queue", "Search queue"],
  ["search_evaluate", "Search compute"],
  ["first_candidate", "First candidate"],
  ["trigger_to_send", "Trigger to send"],
  ["send_ack", "Send ack"],
  ["send_to_feed", "Send to inclusion"],
];

export function LatencyPage({ prometheus }: { prometheus: boolean }) {
  const { selected } = useLive();
  const [source, setSource] = useState("fast");
  const [range, setRange] = useState("1h");
  if (!selected?.sources) return <Empty>No data from this instance yet.</Empty>;
  const view = selected.sources[source] ?? Object.values(selected.sources)[0];
  if (!view) return <Empty>No data from this instance yet.</Empty>;
  return (
    <>
      <header className="page-title">
        <h1>Latency</h1>
        <Segmented label="Source" options={Object.keys(selected.sources).sort().map((name) => ({ value: name, label: sourceLabels[name] ?? name }))} value={source} onChange={setSource} />
      </header>
      <StageTable view={view} />
      <Group title="Trend (p50 and p90)" trailing={<RangePicker value={range} onChange={setRange} />}>
        <div className="charts">
          {charts.map(([chart, title]) => (
            <HistoryChart key={chart} instance={selected.name} chart={chart} range={range} title={title} format={formatSecondsValue} prometheus={prometheus} />
          ))}
        </div>
      </Group>
      <div className="columns">
        {eventGroups.map((group) => (
          <Group key={group.title} title={group.title}>
            <table>
              <thead>
                <tr><th>Event</th><th className="number">Per second</th><th className="number">Total</th></tr>
              </thead>
              <tbody>
                {group.events.map(([event, label]) => (
                  <tr key={event}>
                    <td>{label}</td>
                    <td className="number">{formatRate(view.rates[event])}</td>
                    <td className="number">{formatCount(view.counters[event])}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Group>
        ))}
      </div>
    </>
  );
}

// Stages overlap, so the table deliberately has no total row.
function StageTable({ view }: { view: SourceView }) {
  const rows = stageOrder.filter((stage) => (view.stages[stage]?.lifetime ?? 0) > 0);
  return (
    <Group title="Stage latency (last minute)">
      {rows.length === 0 ? (
        <Empty>No timing samples for this source yet.</Empty>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr><th>Stage</th><th className="number">Samples</th><th className="number">p50</th><th className="number">p90</th><th className="number">p99</th><th className="number">Total samples</th></tr>
            </thead>
            <tbody>
              {rows.map((stage) => {
                const latency = view.stages[stage]!;
                const idle = latency.count === 0;
                return (
                  <tr key={stage}>
                    <td>{stageLabels[stage]}</td>
                    <td className="number">{formatCount(latency.count)}</td>
                    <td className="number">{idle ? "—" : `≈ ${formatDuration(latency.p50_ns)}`}</td>
                    <td className="number">{idle ? "—" : `≈ ${formatDuration(latency.p90_ns)}`}</td>
                    <td className="number">{idle ? "—" : `${latency.overflow ? ">" : "≈"} ${formatDuration(latency.p99_ns)}`}</td>
                    <td className="number">{formatCount(latency.lifetime)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Group>
  );
}
