import type { Tone } from "./components/bits";

export const sourceLabels: Record<string, string> = { standard: "Standard", fast: "Fast" };

export const stageLabels: Record<string, string> = {
  feed_queue: "Feed queue",
  pool_apply: "Pool update",
  search_queue: "Search queue",
  search_evaluate: "Search compute",
  first_candidate: "First candidate",
  candidate_search: "Candidate output",
  reactive_trigger_to_send: "Trigger to send",
  reactive_send_ack: "Send ack",
  reactive_send_to_feed: "Send to inclusion",
  reactive_refresh: "Context refresh",
  receipt_latency: "Receipt handling",
  rpc_block: "RPC blocks",
  rpc_receipts: "RPC receipts",
  rpc_logs: "RPC logs",
  rpc_call: "RPC calls",
  rpc_batch: "RPC batches",
  rpc_other: "RPC other",
};

export const stageOrder = Object.keys(stageLabels);

// Counter names of the reactive path, from search result to settled success.
export const funnel: { event: string; label: string }[] = [
  { event: "candidates", label: "Candidates" },
  { event: "reactive_admitted", label: "Admitted" },
  { event: "reactive_built", label: "Built" },
  { event: "reactive_sent", label: "Sent" },
  { event: "reactive_landed", label: "Landed" },
  { event: "reactive_succeeded", label: "Succeeded" },
];

export const eventGroups: { title: string; events: [string, string][] }[] = [
  {
    title: "Search",
    events: [
      ["search_submitted", "Submitted"],
      ["search_completed", "Completed"],
      ["search_superseded", "Superseded"],
      ["search_canceled", "Canceled"],
      ["search_canceled_overlap", "Canceled on pool overlap"],
      ["search_overtaken", "Overtaken by new frame"],
      ["paths_considered", "Paths considered"],
      ["paths_selected", "Paths selected"],
    ],
  },
  {
    title: "Send",
    events: [
      ["reactive_sent", "Sent"],
      ["reactive_rejected", "Rejected before send"],
      ["plans_held", "Held by gate"],
      ["reactive_cancels", "Cancel transactions"],
      ["reactive_landed", "Landed"],
      ["reactive_reverted", "Reverted"],
    ],
  },
  {
    title: "Errors",
    events: [
      ["block_mismatch", "Block mismatch"],
      ["pool_errors", "Pool errors"],
      ["reconcile_errors", "Reconcile errors"],
      ["search_errors", "Search errors"],
      ["queue_overflow", "Queue overflow"],
      ["stream_invalidations", "Stream invalidations"],
      ["invalid_payloads", "Invalid payloads"],
    ],
  },
];

export const errorEvents = ["block_mismatch", "pool_errors", "reconcile_errors", "search_errors", "discovery_errors", "rpc_http_errors"];

export const auditActions: Record<string, string> = { pause_delivery: "Pause sending", resume_delivery: "Resume sending" };

export const outcomes: Record<string, { tone: Tone; label: string }> = {
  success: { tone: "good", label: "Success" },
  reverted: { tone: "critical", label: "Reverted" },
  cancelled: { tone: "neutral", label: "Cancelled" },
  landed: { tone: "neutral", label: "Landed" },
  pending: { tone: "neutral", label: "Sent" },
  unlanded: { tone: "serious", label: "Not landed" },
};

export const actionLabels: Record<string, string> = { submit: "Submit", replace: "Replace", cancel: "Cancel" };

// A settled send's fees before the receipt is read, or when it never came.
export const feeStatusLabels: Record<string, string> = { pending: "Pending", unknown: "Unknown" };

// Skip reasons the bot names statically. Error-derived reasons show as written.
export const skipLabels: Record<string, string> = {
  version_superseded: "State superseded",
  missing_receive_time: "Missing receive time",
  quote_older_than_lifetime: "Quote expired",
  canonical_lag: "Canonical lag",
  catching_up: "Catching up",
  standard_while_fast_active: "Standard result while fast active",
  context_unavailable: "Context unavailable",
  context_expired: "Context expired",
  context_newer_than_anchor: "Context newer than anchor",
  dispatch_lane_busy: "Nonce lane busy",
  offer_superseded: "Offer superseded",
  verification_superseded: "Verification superseded",
  start_asset_not_configured: "Start asset not configured",
  non_positive_quote: "Non-positive quote",
  pool_unsupported_by_executor: "Pool unsupported by executor",
  pool_missing_from_view: "Pool missing from view",
  below_build_rank: "Below build rank",
  native_value_unknown: "Native value unknown",
  principal_above_limit: "Principal above limit",
  calldata_too_large: "Calldata too large",
  native_balance_short: "Native balance short",
  not_better_than_pending: "Not better than pending",
  no_admissible_opportunity: "No admissible opportunity",
  input_moved_during_build: "Input moved during build",
  context_older_than_anchor: "Context older than anchor",
  other: "Other",
};

// Some reasons carry bucketed details after the name, e.g. "blocks=21-30".
export function skipLabel(reason: string): string {
  const [name = "", ...details] = reason.split(" ");
  return [skipLabels[name] ?? name, ...details].join(" ");
}

export const seriesLabels: Record<string, string> = {
  ...sourceLabels,
  process: "Process",
  p50: "p50",
  p90: "p90",
  sent: "Sent",
  landed: "Landed",
  succeeded: "Succeeded",
  reverted: "Reverted",
  cumulative: "Cumulative net",
};
