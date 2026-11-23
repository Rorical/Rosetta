import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState, type FormEvent } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, ApiError } from "./api";
import { Logo } from "./components/bits";
import { DeliveryControl } from "./components/DeliveryControl";
import { InstancePicker } from "./components/InstancePicker";
import { LiveProvider, useLive } from "./live";
import { AuditPage } from "./pages/AuditPage";
import { CandidatesPage } from "./pages/CandidatesPage";
import { CompetitionPage } from "./pages/CompetitionPage";
import { ExecutionPage } from "./pages/ExecutionPage";
import { LatencyPage } from "./pages/LatencyPage";
import { OverviewPage } from "./pages/OverviewPage";
import { PnLPage } from "./pages/PnLPage";
import { SendsPage } from "./pages/SendsPage";
import { SyncPage } from "./pages/SyncPage";

const destinations = [
  { to: "/", label: "Overview", end: true },
  { to: "/sends", label: "Transactions" },
  { to: "/pnl", label: "PnL" },
  { to: "/execution", label: "Execution" },
  { to: "/competition", label: "Competition" },
  { to: "/candidates", label: "Candidates" },
  { to: "/latency", label: "Latency" },
  { to: "/sync", label: "Sync and nodes" },
  { to: "/audit", label: "Audit" },
];

export function App() {
  const client = useQueryClient();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session, retry: false });
  const expire = useCallback(() => client.setQueryData(["session"], null), [client]);

  if (session.isPending) return null;
  if (!session.data) {
    if (session.error && !(session.error instanceof ApiError && session.error.status === 401)) {
      return <div className="sign-in"><p role="alert">Cannot reach the console service.</p></div>;
    }
    return <SignIn onSignedIn={(data) => client.setQueryData(["session"], data)} />;
  }
  return (
    <LiveProvider onExpired={expire}>
      <Shell prometheus={session.data.prometheus} onSignOut={expire} />
    </LiveProvider>
  );
}

function SignIn({ onSignedIn }: { onSignedIn: (data: { prometheus: boolean }) => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      onSignedIn(await api.signIn(token));
    } catch (failure) {
      setError(failure instanceof ApiError && failure.status === 401 ? "Incorrect token." : "Cannot reach the console service.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="sign-in">
      <form onSubmit={submit}>
        <Logo size={56} />
        <h1>Rosetta</h1>
        <input type="password" aria-label="Operator token" placeholder="Operator token" autoComplete="current-password" autoFocus value={token} onChange={(event) => setToken(event.target.value)} />
        {error && <p role="alert">{error}</p>}
        <button className="primary" disabled={busy || !token}>Sign in</button>
      </form>
    </main>
  );
}

function Shell({ prometheus, onSignOut }: { prometheus: boolean; onSignOut: () => void }) {
  const { connected, selected } = useLive();
  const signOut = async () => {
    await api.signOut();
    onSignOut();
  };
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <Logo size={22} />
          Rosetta
        </div>
        <nav aria-label="Main navigation">
          {destinations.map((item) => (
            <NavLink key={item.to} to={item.to} end={item.end}>
              {item.label}
            </NavLink>
          ))}
        </nav>
        <button className="plain sign-out" onClick={signOut}>Sign out</button>
      </aside>
      <div className="workspace">
        <header className="toolbar">
          <InstancePicker />
          <div className="toolbar-end">
            {!connected && <span className="status serious" role="status">Live connection lost</span>}
            {selected && <DeliveryControl instance={selected} />}
          </div>
        </header>
        <main className="content">
          <Routes>
            <Route path="/" element={<OverviewPage prometheus={prometheus} />} />
            <Route path="/sends" element={<SendsPage />} />
            <Route path="/pnl" element={<PnLPage />} />
            <Route path="/execution" element={<ExecutionPage />} />
            <Route path="/competition" element={<CompetitionPage />} />
            <Route path="/candidates" element={<CandidatesPage />} />
            <Route path="/latency" element={<LatencyPage prometheus={prometheus} />} />
            <Route path="/sync" element={<SyncPage prometheus={prometheus} />} />
            <Route path="/audit" element={<AuditPage />} />
          </Routes>
        </main>
      </div>
    </div>
  );
}
