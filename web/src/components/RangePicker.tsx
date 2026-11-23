import { Segmented } from "./bits";

export const ranges = [
  { value: "15m", label: "15m" },
  { value: "1h", label: "1h" },
  { value: "6h", label: "6h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
];

export function RangePicker({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  return <Segmented label="Time range" options={ranges} value={value} onChange={onChange} />;
}
