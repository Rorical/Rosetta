import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import uPlot from "uplot";
import { api, type History } from "../api";
import { seriesLabels } from "../labels";
import { Empty } from "./bits";

// Source colors were validated with the dataviz palette checker against each
// surface. Other series take the palette in series order.
const sourceColors: Record<string, { light: string; dark: string }> = {
  standard: { light: "#0071E3", dark: "#2E8FF0" },
  fast: { light: "#E8590C", dark: "#DB6A10" },
};
const palette = [
  { light: "#0071E3", dark: "#2E8FF0" },
  { light: "#E8590C", dark: "#DB6A10" },
  { light: "#1A7F37", dark: "#30A46C" },
  { light: "#8E44AD", dark: "#A66BD3" },
];

function usePrefersDark(): boolean {
  const [dark, setDark] = useState(() => matchMedia("(prefers-color-scheme: dark)").matches);
  useEffect(() => {
    const query = matchMedia("(prefers-color-scheme: dark)");
    const update = () => setDark(query.matches);
    query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, []);
  return dark;
}

export function HistoryChart({ instance, chart, range, title, format, prometheus }: { instance: string; chart: string; range: string; title: string; format: (value: number) => string; prometheus: boolean }) {
  const history = useQuery({
    queryKey: ["history", instance, chart, range],
    queryFn: () => api.history(instance, chart, range),
    enabled: prometheus,
    refetchInterval: 15_000,
    placeholderData: (previous) => previous,
  });
  return (
    <figure className="chart">
      <figcaption>{title}</figcaption>
      {!prometheus ? <Empty>History requires -prometheus.</Empty> : history.isError ? <Empty>Cannot read Prometheus.</Empty> : history.data && hasSamples(history.data) ? <Plot history={history.data} format={format} /> : <Empty>{history.isPending ? "" : "No data in this range."}</Empty>}
    </figure>
  );
}

function tickLabel(seconds: number, spansDays: boolean): string {
  const date = new Date(seconds * 1000);
  if (spansDays) return date.toLocaleDateString("en-US", { month: "numeric", day: "numeric" });
  return date.toLocaleTimeString("en-US", { hour12: false, hour: "2-digit", minute: "2-digit" });
}

function hasSamples(history: History): boolean {
  return history.series.some((series) => series.values.some((value) => value !== null));
}

export function Plot({ history, format, days = false }: { history: History; format: (value: number) => string; days?: boolean }) {
  const host = useRef<HTMLDivElement>(null);
  const dark = usePrefersDark();

  useEffect(() => {
    const element = host.current;
    if (!element) return;
    const ink = dark ? "#98989D" : "#6E6E73";
    const grid = dark ? "rgba(235,235,245,0.1)" : "rgba(60,60,67,0.1)";
    const axis: uPlot.Axis = { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { show: false }, font: "11px -apple-system, system-ui, sans-serif" };
    const spansDays = days || (history.timestamps.at(-1) ?? 0) - (history.timestamps[0] ?? 0) > 2 * 86400;
    const lowest = Math.min(0, ...history.series.flatMap((series) => series.values.filter((value): value is number => value !== null)));
    const options: uPlot.Options = {
      width: element.clientWidth,
      height: 180,
      padding: [8, 8, 0, 0],
      cursor: { y: false, drag: { x: false, y: false } },
      legend: { show: true, live: true },
      scales: { x: { time: true }, y: { range: (_plot, _min, max) => [lowest < 0 ? lowest * 1.1 : 0, max > 0 ? max * 1.1 : 1] } },
      axes: [{ ...axis, size: 28, values: (_plot, ticks) => ticks.map((tick) => tickLabel(tick, spansDays)) }, { ...axis, size: 56, values: (_plot, ticks) => ticks.map(format) }],
      series: [
        { label: "Time", value: (_plot, value, _series, index) => (index === null || value === null ? "Latest" : days ? new Date(value * 1000).toLocaleDateString("en-US") : new Date(value * 1000).toLocaleTimeString("en-US", { hour12: false })) },
        ...history.series.map((series, index) => {
          const color = sourceColors[series.name] ?? palette[index % palette.length]!;
          const latest = series.values.findLast((value) => value !== null) ?? null;
          return {
            label: seriesLabels[series.name] ?? series.name,
            stroke: dark ? color.dark : color.light,
            width: 2,
            points: { show: false },
            // With no cursor on the plot the legend reads out the newest value.
            value: (_plot: uPlot, value: number | null, _series: number, index: number | null) => {
              const shown = index === null ? latest : value;
              return shown === null ? "—" : format(shown);
            },
          };
        }),
      ],
    };
    const data = [history.timestamps, ...history.series.map((series) => series.values)] as uPlot.AlignedData;
    const plot = new uPlot(options, data, element);
    const resize = new ResizeObserver(() => plot.setSize({ width: element.clientWidth, height: 180 }));
    resize.observe(element);
    return () => {
      resize.disconnect();
      plot.destroy();
    };
  }, [history, dark, format, days]);

  return <div ref={host} className="plot" />;
}
