import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, ApiError, type InstanceView } from "../api";

// Pausing is the safe direction, so it stays one reasoned step. Resuming lets
// transactions leave the process again, so its sheet spells that out.
export function DeliveryControl({ instance }: { instance: InstanceView }) {
  const [open, setOpen] = useState(false);
  const paused = instance.gate.paused;
  return (
    <>
      {paused && <span className="status warning">Sending paused</span>}
      <button className={paused ? "secondary" : "danger-quiet"} disabled={!instance.online} onClick={() => setOpen(true)}>
        {paused ? "Resume sending" : "Pause sending"}
      </button>
      {open && <GateSheet instance={instance} onClose={() => setOpen(false)} />}
    </>
  );
}

function GateSheet({ instance, onClose }: { instance: InstanceView; onClose: () => void }) {
  const client = useQueryClient();
  const dialog = useRef<HTMLDialogElement>(null);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const resume = instance.gate.paused;

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.setGate(instance.name, !resume, reason);
      await client.invalidateQueries({ queryKey: ["audit"] });
      onClose();
    } catch (failure) {
      setError(failure instanceof ApiError ? failure.message : "Action failed.");
      setBusy(false);
    }
  };

  return (
    <dialog ref={dialog} className="sheet" onClose={onClose}>
      <form onSubmit={submit}>
        <h2>{resume ? `Resume sending on ${instance.name}?` : `Pause sending on ${instance.name}?`}</h2>
        {resume && instance.gate.reason && <p>Paused because: {instance.gate.reason}</p>}
        <p>{resume ? "New transactions and replacements go out immediately." : "Search keeps running. New transactions and replacements stop; cancels of sent transactions continue. The pause survives restarts."}</p>
        <textarea aria-label="Reason" placeholder="Reason" rows={3} maxLength={200} autoFocus value={reason} onChange={(event) => setReason(event.target.value)} />
        {error && <p className="sheet-error" role="alert">{error}</p>}
        <div className="sheet-actions">
          <button type="button" className="secondary" onClick={onClose}>Cancel</button>
          <button className={resume ? "primary" : "danger"} disabled={busy || !reason.trim()}>
            {resume ? "Resume sending" : "Pause sending"}
          </button>
        </div>
      </form>
    </dialog>
  );
}
