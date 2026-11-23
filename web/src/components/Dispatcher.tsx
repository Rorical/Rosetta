import type { DispatchView, InstanceView } from "../api";
import { formatCount, formatEth, formatTime } from "../format";
import { counter, gauge } from "../metrics";
import { funnel } from "../labels";
import { Group, Hex, Row, Status, type Tone } from "./bits";

export function dispatchState(instance: InstanceView, dispatch?: DispatchView): { tone: Tone; label: string } {
  if (gauge(instance, "dispatch_halted") === 1 || dispatch?.halted_reason) return { tone: "critical", label: "Halted" };
  if (gauge(instance, "dispatch_stopped") === 1 || dispatch?.stopped) return { tone: "critical", label: "Stop file active" };
  if (instance.gate.paused) return { tone: "warning", label: "Sending paused" };
  if (dispatch?.pending) return { tone: "neutral", label: "Awaiting inclusion" };
  if (!dispatch) return { tone: "neutral", label: "—" };
  return { tone: "good", label: "Idle" };
}

// The gauges refresh every second; the record adds the exact wei amounts, the
// pending hash and the halt reason.
export function DispatcherGroup({ instance, dispatch, full = false }: { instance: InstanceView; dispatch?: DispatchView; full?: boolean }) {
  const state = dispatchState(instance, dispatch);
  const wallet = gauge(instance, "wallet_native_gwei");
  return (
    <Group title="Dispatcher">
      <Row label="State"><Status tone={state.tone}>{state.label}</Status></Row>
      {dispatch?.halted_reason && <Row label="Halt reason">{dispatch.halted_reason}</Row>}
      <Row label="Next nonce">{dispatch ? formatCount(dispatch.next_nonce) : "—"}</Row>
      <Row label="Pending">{dispatch?.pending ? <><Hex value={dispatch.pending} />{dispatch.variants > 1 ? ` · ${dispatch.variants} variants` : ""}</> : "None"}</Row>
      {full && <Row label="Unconfirmed">{dispatch ? formatCount(dispatch.included) : "—"}</Row>}
      <Row label="Fees today">{dispatch ? `${formatEth(dispatch.spent_today)} / ${formatEth(dispatch.daily_cap)}` : "—"}</Row>
      {full && <Row label="Per-transaction cap">{dispatch ? formatEth(dispatch.transaction_cap) : "—"}</Row>}
      <Row label="Wallet balance">{wallet ? formatEth((BigInt(wallet) * 1_000_000_000n).toString()) : "—"}</Row>
      {full && dispatch && <Row label="Updated">{formatTime(dispatch.at_ms)}</Row>}
    </Group>
  );
}

function sharePercent(part: number, whole: number): string {
  const share = (part / whole) * 100;
  return `${share.toFixed(share > 0 && share < 1 ? 2 : 1)}%`;
}

// Counts are this run's totals; each step shows its share of the step before.
export function Funnel({ instance }: { instance: InstanceView }) {
  const counts = funnel.map((step) => counter(instance, step.event));
  return (
    <div className="chain" aria-label="Send funnel for this run">
      <ol>
        {funnel.map((step, index) => {
          const previous = index > 0 ? counts[index - 1]! : 0;
          return (
            <li key={step.event}>
              <strong>{formatCount(counts[index])}</strong>
              <span>{step.label}</span>
              <small title="Share of previous step">{previous > 0 ? sharePercent(counts[index]!, previous) : " "}</small>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
