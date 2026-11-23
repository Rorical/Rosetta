import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api";
import { Empty, Group, Hex, Segmented } from "../components/bits";
import { formatCount, formatGwei, formatTime } from "../format";
import { useLive } from "../live";

const windows = [
  { value: "1h", label: "1h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
];

export function CompetitionPage() {
  const { selected } = useLive();
  const [range, setRange] = useState("24h");
  const name = selected?.name ?? "";
  const competition = useQuery({
    queryKey: ["competition", name, range],
    queryFn: () => api.competition(name, range),
    enabled: Boolean(selected),
    refetchInterval: 15_000,
    placeholderData: (previous) => previous,
  });
  const data = competition.data;
  return (
    <>
      <header className="page-title">
        <h1>Competition</h1>
        <Segmented label="Time range" options={windows} value={range} onChange={setRange} />
      </header>
      <div className="kpis">
        <div><strong>{formatCount(data?.transactions)}</strong><span>Rival transactions</span></div>
        <div><strong>{formatCount(data?.senders)}</strong><span>Senders</span></div>
      </div>
      <Group title="Top rivals">
        {competition.isError ? (
          <Empty>Cannot read competition records.</Empty>
        ) : data?.rivals.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Sender</th><th className="number">Transactions</th><th className="number">Success</th><th className="number">Avg tip</th>
                  <th className="number">Max tip</th><th className="number">Avg landing frame</th><th>Last seen</th>
                </tr>
              </thead>
              <tbody>
                {data.rivals.map((rival) => (
                  <tr key={rival.sender}>
                    <td><Hex value={rival.sender} /></td>
                    <td className="number">{formatCount(rival.transactions)}</td>
                    <td className="number">{formatCount(rival.succeeded)}</td>
                    <td className="number">{formatGwei(rival.average_tip)}</td>
                    <td className="number">{formatGwei(rival.max_tip)}</td>
                    <td className="number">{rival.average_frame.toFixed(1)}</td>
                    <td>{formatTime(rival.last_ms)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty>{competition.isPending ? "" : "No rival transactions in this range."}</Empty>
        )}
      </Group>
    </>
  );
}
