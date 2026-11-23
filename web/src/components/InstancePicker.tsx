import { useLive } from "../live";
import { StatusDot } from "./bits";

export function InstancePicker() {
  const { instances, selected, select } = useLive();
  if (!selected) return <span className="instance-name">Waiting for instance</span>;
  return (
    <div className="instance-name">
      <StatusDot tone={selected.online ? "good" : "critical"} label={selected.online ? "Online" : "Offline"} />
      {instances.length > 1 ? (
        <select aria-label="Instance" value={selected.name} onChange={(event) => select(event.target.value)}>
          {instances.map((instance) => (
            <option key={instance.name} value={instance.name}>
              {instance.name}
            </option>
          ))}
        </select>
      ) : (
        selected.name
      )}
    </div>
  );
}
