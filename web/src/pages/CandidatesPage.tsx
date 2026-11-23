import { useQuery } from "@tanstack/react-query";
import { Fragment, useState } from "react";
import { api, type Candidate } from "../api";
import { Empty, Group, Hex, Segmented } from "../components/bits";
import { RouteKinds } from "../components/SendsTable";
import { formatCount, formatEth, formatTime } from "../format";
import { sourceLabels } from "../labels";
import { useLive } from "../live";
import { useUnits, type Units } from "../units";

// Gross is the search's own result before network cost. Research's estimated
// net charges other senders' observed transaction costs per hop, far above
// ours, so it appears only in the detail and under its own name.
export function CandidatesPage() {
  const { selected } = useLive();
  const [source, setSource] = useState("fast");
  const [sort, setSort] = useState("recent");
  const [open, setOpen] = useState<number | null>(null);
  const name = selected?.name ?? "";
  const rows = useQuery({
    queryKey: ["candidates", name, source, sort],
    queryFn: () => api.candidates(name, { source: source || undefined, sort }),
    enabled: Boolean(selected),
    refetchInterval: 3_000,
    placeholderData: (previous) => previous,
  });
  const units = useUnits(selected?.name);
  return (
    <>
      <header className="page-title">
        <h1>Candidates</h1>
      </header>
      <div className="filters">
        <Segmented label="Source" options={[{ value: "fast", label: "Fast" }, { value: "standard", label: "Standard" }, { value: "", label: "All" }]} value={source} onChange={setSource} />
        <Segmented label="Sort" options={[{ value: "recent", label: "Recent" }, { value: "gross", label: "Top gross" }]} value={sort} onChange={setSort} />
      </div>
      <Group>
        {rows.isError ? (
          <Empty>Cannot read candidates.</Empty>
        ) : rows.data?.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Time</th><th>Source</th><th className="number">Block</th><th>Route</th>
                  <th className="number">Input</th><th className="number">Gross</th><th className="number">Gross ≈</th>
                </tr>
              </thead>
              <tbody>
                {rows.data.map((row) => (
                  <Fragment key={row.id}>
                    <tr className="expandable" onClick={() => setOpen(open === row.id ? null : row.id)}>
                      <td>
                        <button className="disclosure" aria-expanded={open === row.id} aria-label="Show details" />
                        {formatTime(row.recorded_ms)}
                      </td>
                      <td>{sourceLabels[row.source] ?? row.source}</td>
                      <td className="number">{formatCount(row.block)}</td>
                      <td><span className="hops">{row.hops}</span><RouteKinds route={row.route} /></td>
                      <td className="number">{units.amount(row.input, row.asset)}</td>
                      <td className="number">{units.amount(row.gross, row.asset)}</td>
                      <td className="number">{row.gross_native === undefined ? "—" : formatEth(row.gross_native)}</td>
                    </tr>
                    {open === row.id && <Detail row={row} units={units} />}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty>{rows.isPending ? "" : "No candidates. Start the instance with -candidate-out."}</Empty>
        )}
      </Group>
    </>
  );
}

function Detail({ row, units }: { row: Candidate; units: Units }) {
  return (
    <tr className="detail">
      <td colSpan={7}>
        <dl>
          <dt>Start asset</dt>
          <dd>{units.symbol(row.asset)} <Hex value={row.asset} /></dd>
          {row.research && (
            <>
              <dt>Research estimate (pessimistic)</dt>
              <dd>
                Net {units.amount(row.research.net, row.asset)} · network fee {units.amount(row.research.network_fee, row.asset)} (at other senders' on-chain cost, {row.research.basis})
              </dd>
            </>
          )}
          <dt>Pools</dt>
          <dd className="route">
            {row.route.map((pool, index) => (
              <span key={`${pool.address}-${index}`}>{pool.kind} <Hex value={pool.address} /></span>
            ))}
          </dd>
        </dl>
      </td>
    </tr>
  );
}
