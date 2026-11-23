import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { Empty, Group, Status, type Tone } from "../components/bits";
import { formatTime } from "../format";
import { auditActions } from "../labels";

const outcomes: Record<string, { tone: Tone; label: string }> = {
  applied: { tone: "good", label: "Applied" },
  failed: { tone: "critical", label: "Failed" },
  pending: { tone: "warning", label: "Unconfirmed" },
};

export function AuditPage() {
  const entries = useQuery({ queryKey: ["audit"], queryFn: api.audit, refetchInterval: 10_000 });
  return (
    <>
      <header className="page-title">
        <h1>Audit</h1>
      </header>
      <Group>
        {entries.isError ? (
          <Empty>Cannot read the audit log.</Empty>
        ) : entries.data?.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr><th>Time</th><th>Instance</th><th>Action</th><th>Reason</th><th>Result</th></tr>
              </thead>
              <tbody>
                {entries.data.map((entry) => {
                  const outcome = outcomes[entry.outcome] ?? { tone: "neutral" as Tone, label: entry.outcome };
                  return (
                    <tr key={entry.id}>
                      <td>{formatTime(entry.at_ms)}</td>
                      <td>{entry.instance}</td>
                      <td>{auditActions[entry.action] ?? entry.action}</td>
                      <td className="wrap">{entry.reason}</td>
                      <td title={entry.error}><Status tone={outcome.tone}>{outcome.label}</Status></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty>{entries.isPending ? "" : "No actions yet."}</Empty>
        )}
      </Group>
    </>
  );
}
