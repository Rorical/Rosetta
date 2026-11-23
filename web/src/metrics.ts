import type { InstanceView, StageView } from "./api";

// counter sums one event over every source of the run.
export function counter(instance: InstanceView, event: string): number {
  return Object.values(instance.sources ?? {}).reduce((total, source) => total + (source.counters[event] ?? 0), 0);
}

// Process-wide gauges such as the dispatcher's are reported on the standard source.
export function gauge(instance: InstanceView, name: string, source = "standard"): number | undefined {
  return instance.sources?.[source]?.gauges[name];
}

// stage picks the source that observed the stage most; on Flashblocks that is
// the fast source, since canonical search is skipped.
export function stage(instance: InstanceView, name: string): StageView | undefined {
  let best: StageView | undefined;
  for (const source of Object.values(instance.sources ?? {})) {
    const candidate = source.stages[name];
    if (candidate && candidate.lifetime > (best?.lifetime ?? 0)) best = candidate;
  }
  return best;
}

export function utcDay(milliseconds: number): string {
  return new Date(milliseconds).toISOString().slice(0, 10);
}
