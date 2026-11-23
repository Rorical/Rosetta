import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { DispatcherGroup, Funnel } from "../components/Dispatcher";
import { Empty, Group } from "../components/bits";
import { formatCount, formatTime } from "../format";
import { skipLabel } from "../labels";
import { useLive } from "../live";
import { counter } from "../metrics";

const percent = (part: number, whole: number) => (whole > 0 ? `${((part / whole) * 100).toFixed(1)}%` : "—");

export function ExecutionPage() {
  const { selected } = useLive();
  const name = selected?.name ?? "";
  const started = selected?.status?.started_at_ms ?? 0;
  const execution = useQuery({ queryKey: ["execution", name, started], queryFn: () => api.execution(name, started), enabled: Boolean(selected), refetchInterval: 5_000 });
  if (!selected) return <Empty>Connecting to instance.</Empty>;
  if (execution.isError) return <Empty>Cannot read execution records.</Empty>;
  const data = execution.data;
  const skipped = (data?.skips ?? []).reduce((total, skip) => total + skip.count, 0);
  return (
    <>
      <header className="page-title">
        <h1>Execution</h1>
      </header>
      {selected.sources && <Funnel instance={selected} />}
      <div className="columns">
        <DispatcherGroup instance={selected} dispatch={data?.dispatch} full />
        <Group title="Send results (this run)">
          <table>
            <tbody>
              {[
                ["reactive_sent", "Sent"],
                ["reactive_rejected", "Rejected before send"],
                ["plans_held", "Held by gate"],
                ["reactive_cancels", "Cancel transactions"],
                ["reactive_landed", "Landed"],
                ["reactive_succeeded", "Succeeded"],
                ["reactive_reverted", "Reverted"],
              ].map(([event, label]) => (
                <tr key={event}>
                  <td>{label}</td>
                  <td className="number">{formatCount(counter(selected, event!))}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Group>
      </div>
      <Group title="Skip reasons (this run)">
        {data?.skips.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr><th>Reason</th><th className="number">Count</th><th className="number">Share</th></tr>
              </thead>
              <tbody>
                {data.skips.map((skip) => (
                  <tr key={skip.reason}>
                    <td className="wrap" title={skip.reason}>{skipLabel(skip.reason)}</td>
                    <td className="number">{formatCount(skip.count)}</td>
                    <td className="number">{percent(skip.count, skipped)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty>{execution.isPending ? "" : "No skips this run."}</Empty>
        )}
      </Group>
      <div className="columns">
        <Group title="Route survival by hops" trailing={data?.survival ? <span className="caption">{formatTime(data.survival.at_ms)}</span> : undefined}>
          {data?.survival?.rows.length ? (
            <table>
              <thead>
                <tr><th>Hops</th><th className="number">Survived / total</th><th className="number">Survival rate</th></tr>
              </thead>
              <tbody>
                {data.survival.rows.map((row) => (
                  <tr key={row.hops}>
                    <td>{row.hops >= 6 ? "6+" : row.hops}</td>
                    <td className="number">{formatCount(row.survived)} / {formatCount(row.total)}</td>
                    <td className="number">{percent(row.survived, row.total)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <Empty>{execution.isPending ? "" : "No execution paper check yet."}</Empty>
          )}
        </Group>
        <Group title="Send paths (24h)">
          {data?.paths.length ? (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr><th>Path</th><th className="number">Sent</th><th className="number">Accepted</th><th className="number">p50</th><th className="number">p90</th><th className="number">Max</th></tr>
                </thead>
                <tbody>
                  {data.paths.map((path) => (
                    <tr key={path.name}>
                      <td className="wrap">{path.name}</td>
                      <td className="number">{formatCount(path.sends)}</td>
                      <td className="number">{formatCount(path.accepted)}</td>
                      <td className="number">{path.p50_ms} ms</td>
                      <td className="number">{path.p90_ms} ms</td>
                      <td className="number">{path.max_ms} ms</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty>{execution.isPending ? "" : "No multi-path sends configured."}</Empty>
          )}
        </Group>
      </div>
    </>
  );
}
