import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api";
import { Empty, Group, Segmented } from "../components/bits";
import { SendsTable } from "../components/SendsTable";
import { useLive } from "../live";
import { useUnits } from "../units";

const filters = [
  { value: "", label: "All" },
  { value: "success", label: "Success" },
  { value: "reverted", label: "Reverted" },
  { value: "unlanded", label: "Not landed" },
];

export function SendsPage() {
  const { selected } = useLive();
  const [outcome, setOutcome] = useState("");
  const name = selected?.name ?? "";
  const sends = useQuery({
    queryKey: ["sends", name, outcome, 200],
    queryFn: () => api.sends(name, outcome, 200),
    enabled: Boolean(selected),
    refetchInterval: 3_000,
    placeholderData: (previous) => previous,
  });
  const units = useUnits(selected?.name);
  return (
    <>
      <header className="page-title">
        <h1>Transactions</h1>
        <Segmented label="Outcome" options={filters} value={outcome} onChange={setOutcome} />
      </header>
      <Group>
        {sends.isError ? (
          <Empty>Cannot read transactions.</Empty>
        ) : sends.data?.length ? (
          <SendsTable sends={sends.data} units={units} />
        ) : (
          <Empty>{sends.isPending ? "" : outcome ? "No matching transactions." : "No transactions sent yet."}</Empty>
        )}
      </Group>
    </>
  );
}
