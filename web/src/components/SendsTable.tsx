import { Fragment, useState, type ReactNode } from "react";
import type { PoolRef, Send } from "../api";
import { formatCount, formatEth, formatGwei, formatMilliseconds, formatTime } from "../format";
import { actionLabels, feeStatusLabels, outcomes, sourceLabels } from "../labels";
import type { Units } from "../units";
import { Hex, Status } from "./bits";

export function RouteKinds({ route }: { route: PoolRef[] }) {
  return <span className="route-kinds">{route.map((pool) => pool.kind).join(" → ")}</span>;
}

function OutcomeStatus({ send }: { send: Send }) {
  const outcome = outcomes[send.outcome] ?? { tone: "neutral" as const, label: send.outcome };
  return <Status tone={outcome.tone}>{outcome.label}</Status>;
}

// The outcome is booked from the feed; net and fees wait for the receipt.
function unpaid(send: Send): string | undefined {
  const status = send.settled?.fee_status;
  return status ? (feeStatusLabels[status] ?? status) : undefined;
}

function landingText(send: Send): string {
  if (!send.landing || send.landing.invalidated) return "—";
  const blocks = send.landing.block - send.trigger_block;
  return `+${blocks} ${blocks === 1 ? "block" : "blocks"} · frame ${send.landing.frame}`;
}

export function SendsTable({ sends, units, compact = false }: { sends: Send[]; units: Units; compact?: boolean }) {
  const [open, setOpen] = useState<string | null>(null);
  const columns = compact ? 5 : 8;
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Outcome</th>
            <th>Route</th>
            {!compact && <th className="number">Input</th>}
            {!compact && <th className="number">Expected net</th>}
            <th className="number">Actual net</th>
            {!compact && <th className="number">Fees</th>}
            <th>Landing</th>
          </tr>
        </thead>
        <tbody>
          {sends.map((send) => (
            <Fragment key={send.hash}>
              <tr className="expandable" onClick={() => setOpen(open === send.hash ? null : send.hash)}>
                <td>
                  <button className="disclosure" aria-expanded={open === send.hash} aria-label="Show details" />
                  {formatTime(send.sent_ms)}
                </td>
                <td><OutcomeStatus send={send} /></td>
                <td><span className="hops">{send.route.length}</span><RouteKinds route={send.route} /></td>
                {!compact && <td className="number">{units.amount(send.input, send.asset)}</td>}
                {!compact && <td className="number">{units.amount(send.expected_net, send.asset)}</td>}
                <td className="number">
                  {!send.settled ? "—" : unpaid(send) ?? <Status tone={send.settled.net.startsWith("-") ? "critical" : "good"}>{units.amount(send.settled.net, send.asset)}</Status>}
                </td>
                {!compact && <td className="number">{send.settled ? unpaid(send) ?? formatEth(send.settled.fee) : "—"}</td>}
                <td>{landingText(send)}</td>
              </tr>
              {open === send.hash && (
                <tr className="detail">
                  <td colSpan={columns}><SendDetail send={send} units={units} /></td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </>
  );
}

function SendDetail({ send, units }: { send: Send; units: Units }) {
  const settled = send.settled;
  const landing = send.landing;
  return (
    <dl>
      <Item label="Transaction"><Hex value={send.hash} /> · {actionLabels[send.action] ?? send.action}</Item>
      <Item label="Trigger">Block {formatCount(send.trigger_block)} · {sourceLabels[send.trigger_source] ?? send.trigger_source}{send.lane ? ` · ${send.lane}` : ""}</Item>
      {send.received_ns > 0 && <Item label="Trigger to send">{formatMilliseconds(send.execute_ns - send.received_ns)}</Item>}
      <Item label="Send ack">
        {send.paths.length > 0 ? (
          <span className="route">
            {send.paths.map((path) => (
              <span key={path.name} title={path.error}>
                {path.name} {path.elapsed_ms} ms <Status tone={path.accepted ? "good" : "neutral"}>{path.accepted ? "Accepted" : "Not accepted"}</Status>
              </span>
            ))}
          </span>
        ) : (
          formatMilliseconds(send.ack_ns)
        )}
      </Item>
      {landing && (
        <Item label="Landing position">
          Block {formatCount(landing.block)} · frame {landing.frame} · index {landing.index} · {formatMilliseconds(landing.send_to_feed_ns)} after send
          {landing.invalidated ? " · stream reset" : ""}
        </Item>
      )}
      {settled && <Item label="Landed block">{formatCount(settled.block)} · {formatTime(settled.at_ms)}</Item>}
      <Item label="Quoted gross">{units.amount(send.quoted_gross, send.asset)}</Item>
      {settled && <Item label="Actual gross">{units.amount(settled.profit, send.asset)}</Item>}
      {settled?.net_native !== undefined && <Item label="Actual net ≈">{formatEth(settled.net_native)}</Item>}
      {settled && <Item label="Fees">{unpaid(send) ?? <>{formatEth(settled.fee)}{settled.l1_fee ? ` · L1 ${formatEth(settled.l1_fee)}` : ""}</>}</Item>}
      <Item label="Fee budget">{formatEth(send.budget)}</Item>
      <Item label="Tip">{formatGwei(send.tip)}</Item>
      {send.bid && (
        <Item label="Bid model">
          {formatGwei(send.bid.model_tip)} ·{" "}
          {send.bid.win_probability !== undefined ? `win rate ${Math.round(send.bid.win_probability * 100)}%` : `wins ${send.bid.wins}/${send.bid.samples}`}
          {send.bid.probe ? " · probe" : ""} · {send.bid.applied ? "applied" : "logged only"}
        </Item>
      )}
      <Item label="Gas">{settled && !unpaid(send) ? `${formatCount(settled.gas_used)} / ` : ""}{formatCount(send.gas_limit)}</Item>
      {send.rival && (
        <Item label="Front-runner">
          <Hex value={send.rival.sender} /> · block {formatCount(send.rival.block)} frame {send.rival.frame} · {formatGwei(send.rival.tip)}
        </Item>
      )}
      {send.error && <Item label="Error">{send.error}</Item>}
      <Item label="Pools">
        <span className="route">
          {send.route.map((pool, index) => (
            <span key={`${pool.address}-${index}`}>{pool.kind} <Hex value={pool.address} /></span>
          ))}
        </span>
      </Item>
    </dl>
  );
}
