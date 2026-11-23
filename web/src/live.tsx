import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { InstanceView } from "./api";

interface LiveState {
  instances: InstanceView[];
  connected: boolean;
  selected?: InstanceView;
  select: (name: string) => void;
}

const LiveContext = createContext<LiveState | null>(null);
const selectionKey = "rosetta.instance";

export function LiveProvider({ children, onExpired }: { children: ReactNode; onExpired: () => void }) {
  const [instances, setInstances] = useState<InstanceView[]>([]);
  const [connected, setConnected] = useState(false);
  const [name, setName] = useState(() => localStorage.getItem(selectionKey) ?? "");

  useEffect(() => {
    const events = new EventSource("/api/events");
    events.onopen = () => setConnected(true);
    events.onmessage = (message) => setInstances(JSON.parse(message.data));
    events.onerror = () => {
      setConnected(false);
      // EventSource hides the status code, so ask whether the session ended.
      fetch("/api/session").then((response) => {
        if (response.status === 401) onExpired();
      });
    };
    return () => events.close();
  }, [onExpired]);

  const value = useMemo<LiveState>(
    () => ({
      instances,
      connected,
      selected: instances.find((instance) => instance.name === name) ?? instances[0],
      select: (next) => {
        localStorage.setItem(selectionKey, next);
        setName(next);
      },
    }),
    [instances, connected, name],
  );
  return <LiveContext.Provider value={value}>{children}</LiveContext.Provider>;
}

export function useLive(): LiveState {
  const state = useContext(LiveContext);
  if (!state) throw new Error("useLive outside LiveProvider");
  return state;
}
