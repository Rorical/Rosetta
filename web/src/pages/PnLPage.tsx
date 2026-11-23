import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { api, type History, type PnLRow } from "../api";
import { Empty, Group, Hex, Status } from "../components/bits";
import { Plot } from "../components/HistoryChart";
import { formatCount, formatEth } from "../format";
import { useLive } from "../live";
import { utcDay } from "../metrics";
import { useUnits } from "../units";

const ethAxis = (wei: number) => `${Number((wei / 1e18).toPrecision(3))}`;

function cumulative(rows: PnLRow[]): History {
  const byDay = new Map<string, number>();
  for (const row of rows) byDay.set(row.day, (byDay.get(row.day) ?? 0) + row.net_native);
  const days = [...byDay.keys()].sort();
  let total = 0;
  return {
    timestamps: days.map((day) => Date.parse(`${day}T00:00:00Z`) / 1000),
    series: [{ name: "cumulative", values: days.map((day) => (total += byDay.get(day)!)) }],
  };
}

function since(rows: PnLRow[], day: string): number {
  return rows.filter((row) => row.day >= day).reduce((total, row) => total + row.net_native, 0);
}

function Signed({ wei }: { wei: number }) {
  return <Status tone={wei < 0 ? "critical" : "good"}>{formatEth(wei)}</Status>;
}

export function PnLPage() {
  const { selected } = useLive();
  const name = selected?.name ?? "";
  const pnl = useQuery({ queryKey: ["pnl", name], queryFn: () => api.pnl(name), enabled: Boolean(selected), refetchInterval: 10_000 });
  const units = useUnits(selected?.name);
  const rows = pnl.data ?? [];
  const history = useMemo(() => cumulative(rows), [rows]);
  if (pnl.isError) return <Empty>Cannot read PnL records.</Empty>;
  const now = Date.now();
  return (
    <>
      <header className="page-title">
        <h1>PnL</h1>
      </header>
      <div className="kpis">
        <div><strong><Signed wei={since(rows, utcDay(now))} /></strong><span>Net today (UTC)</span></div>
        <div><strong><Signed wei={since(rows, utcDay(now - 6 * 86_400_000))} /></strong><span>Last 7 days</span></div>
        <div><strong><Signed wei={since(rows, "")} /></strong><span>All time</span></div>
      </div>
      <Group title="Cumulative net (ETH)">
        {history.timestamps.length > 1 ? <div className="chart"><Plot history={history} format={ethAxis} days /></div> : <Empty>{pnl.isPending ? "" : "Not enough days yet."}</Empty>}
      </Group>
      <Group title="By day and asset">
        {rows.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Date</th><th>Asset</th><th className="number">Settled</th><th className="number">Success</th><th className="number">Reverted</th>
                  <th className="number">Gross</th><th className="number">Fees</th><th className="number">Net</th><th className="number">Net ≈</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr key={`${row.day}-${row.asset}`}>
                    <td>{row.day}</td>
                    <td title={row.asset}>{units.symbol(row.asset).startsWith("0x") ? <Hex value={row.asset} /> : units.symbol(row.asset)}</td>
                    <td className="number">{formatCount(row.settled)}</td>
                    <td className="number">{formatCount(row.succeeded)}</td>
                    <td className="number">{formatCount(row.reverted)}</td>
                    <td className="number">{units.value(row.profit, row.asset)}</td>
                    <td className="number">{formatEth(row.fee)}</td>
                    <td className="number">{units.value(row.net, row.asset)}</td>
                    <td className="number"><Signed wei={row.net_native} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty>{pnl.isPending ? "" : "No settled transactions yet."}</Empty>
        )}
      </Group>
    </>
  );
}
