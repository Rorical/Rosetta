export interface StageView {
  count: number;
  lifetime: number;
  p50_ns: number;
  p90_ns: number;
  p99_ns: number;
  overflow: boolean;
}

export interface SourceView {
  counters: Record<string, number>;
  gauges: Record<string, number>;
  estimates: Record<string, number>;
  rates: Record<string, number>;
  stages: Record<string, StageView>;
}

export interface Status {
  run_id: string;
  network: string;
  chain_id: number;
  backend: string;
  event_source: string;
  version: string;
  started_at_ms: number;
  outputs: { candidates?: string; reactive?: string; competition?: string };
}

export interface GateState {
  paused: boolean;
  reason: string;
  actor: string;
  changed_at_ms: number;
}

export interface Process {
  goroutines: number;
  heap_bytes: number;
  memory_bytes: number;
  gc_cycles: number;
  gc_cpu_seconds: number;
  gc_pause_seconds: number;
  cpu_seconds: number;
  open_files: number;
}

export interface InstanceView {
  name: string;
  online: boolean;
  error?: string;
  last_seen_ms: number;
  status?: Status;
  gate: GateState;
  process: Process;
  cpu_percent: number;
  sources?: Record<string, SourceView>;
}

export interface PoolRef {
  address: string;
  kind: string;
}

export interface PathResult {
  name: string;
  elapsed_ms: number;
  accepted: boolean;
  error?: string;
}

export type Outcome = "success" | "reverted" | "cancelled" | "landed" | "pending" | "unlanded";

export interface Send {
  hash: string;
  intent_id: string;
  action: string;
  sent_ms: number;
  asset: string;
  input: string;
  expected_net: string;
  quoted_gross: string;
  budget: string;
  tip: string;
  gas_limit: number;
  conv_num: string;
  conv_den: string;
  bid?: { model_tip: string; wins: number; samples: number; applied: boolean; win_probability?: number; probe?: boolean };
  route: PoolRef[];
  trigger_block: number;
  trigger_source: string;
  lane: string;
  received_ns: number;
  execute_ns: number;
  ack_ns?: number;
  error: string;
  paths: PathResult[];
  landing?: { block: number; frame: number; index: number; received_ns: number; send_to_feed_ns?: number; success: boolean; invalidated: boolean };
  settled?: { block: number; at_ms: number; success: boolean; cancelled: boolean; profit: string; net: string; fee: string; l1_fee: string; gas_used: number; net_native?: number; fee_status?: "pending" | "unknown" };
  outcome: Outcome;
  rival?: { sender: string; hash: string; block: number; frame: number; index: number; tip: string };
}

export interface PnLRow {
  day: string;
  asset: string;
  settled: number;
  succeeded: number;
  reverted: number;
  cancelled: number;
  profit: number;
  net: number;
  fee: number;
  profit_native: number;
  net_native: number;
}

export interface DispatchView {
  at_ms: number;
  halted_reason: string;
  stopped: boolean;
  next_nonce: number;
  pending: string;
  variants: number;
  included: number;
  spent_today: string;
  daily_cap: string;
  transaction_cap: string;
}

export interface Execution {
  dispatch?: DispatchView;
  skips: { reason: string; count: number }[];
  survival?: { at_ms: number; rows: { hops: number; survived: number; total: number }[] };
  paths: { name: string; sends: number; accepted: number; p50_ms: number; p90_ms: number; max_ms: number }[];
}

export interface Competition {
  transactions: number;
  senders: number;
  rivals: { sender: string; transactions: number; succeeded: number; average_tip: number; max_tip: number; average_frame: number; last_ms: number }[];
}

export interface Asset {
  token: string;
  symbol: string;
  decimals: number;
}

export interface Candidate {
  id: number;
  recorded_ms: number;
  source: string;
  block: number;
  asset: string;
  input: string;
  gross: string;
  gross_native?: number;
  hops: number;
  route: PoolRef[];
  research?: { net: string; network_fee: string; basis: string };
}

export interface AuditEntry {
  id: number;
  at_ms: number;
  actor: string;
  instance: string;
  action: string;
  reason: string;
  outcome: string;
  error: string;
}

export interface Alert {
  name: string;
  severity: string;
  state: string;
  instance: string;
  summary: string;
  active_at: string;
}

export interface History {
  timestamps: number[];
  series: { name: string; values: (number | null)[] }[];
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { credentials: "same-origin", ...init });
  if (response.status === 204) return undefined as T;
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new ApiError(response.status, body?.error ?? `Request failed (${response.status})`);
  return body as T;
}

function write<T>(path: string, method: string, body?: unknown): Promise<T> {
  return request<T>(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
}

function query(parameters: Record<string, string | undefined>): string {
  const entries = Object.entries(parameters).filter((entry): entry is [string, string] => Boolean(entry[1]));
  return entries.length ? `?${new URLSearchParams(entries)}` : "";
}

export const api = {
  session: () => request<{ prometheus: boolean }>("/api/session"),
  signIn: (token: string) => write<{ prometheus: boolean }>("/api/session", "POST", { token }),
  signOut: () => write<void>("/api/session", "DELETE"),
  setGate: (instance: string, paused: boolean, reason: string) => write<GateState>(`/api/instances/${instance}/gate`, "PUT", { paused, reason }),
  history: (instance: string, chart: string, range: string) => request<History>(`/api/instances/${instance}/history/${chart}?range=${range}`),
  alerts: () => request<Alert[]>("/api/alerts"),
  audit: () => request<AuditEntry[]>("/api/audit?limit=200"),
  sends: (instance: string, outcome: string, limit: number) => request<Send[]>(`/api/instances/${instance}/sends${query({ outcome, limit: String(limit) })}`),
  pnl: (instance: string) => request<PnLRow[]>(`/api/instances/${instance}/pnl`),
  execution: (instance: string, since: number) => request<Execution>(`/api/instances/${instance}/execution?since=${since}`),
  competition: (instance: string, range: string) => request<Competition>(`/api/instances/${instance}/competition?range=${range}&limit=50`),
  assets: (instance: string) => request<Asset[]>(`/api/instances/${instance}/assets`),
  candidates: (instance: string, filter: { source?: string; sort: string }) => request<Candidate[]>(`/api/instances/${instance}/candidates${query({ ...filter, limit: "200" })}`),
};
