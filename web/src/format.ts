const grouped = new Intl.NumberFormat("en-US");

export function formatCount(value: number | undefined): string {
  return value === undefined ? "—" : grouped.format(value);
}

export function formatCompact(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1e9) return `${trim(value / 1e9)}B`;
  if (abs >= 1e6) return `${trim(value / 1e6)}M`;
  if (abs >= 1e4) return `${trim(value / 1e3)}K`;
  return grouped.format(Math.round(value));
}

function trim(value: number): string {
  return value.toFixed(Math.abs(value) >= 100 ? 0 : 1);
}

export function formatRate(value: number | undefined): string {
  if (value === undefined) return "—";
  if (value === 0) return "0";
  if (Math.abs(value) >= 100) return formatCompact(value);
  if (Math.abs(value) >= 1) return value.toFixed(1);
  return value.toFixed(2);
}

export function formatDuration(nanoseconds: number): string {
  if (nanoseconds < 1_000) return `${Math.round(nanoseconds)} ns`;
  if (nanoseconds < 1_000_000) return `${(nanoseconds / 1_000).toFixed(nanoseconds < 10_000 ? 1 : 0)} µs`;
  if (nanoseconds < 1_000_000_000) return `${(nanoseconds / 1_000_000).toFixed(nanoseconds < 10_000_000 ? 1 : 0)} ms`;
  return `${(nanoseconds / 1_000_000_000).toFixed(2)} s`;
}

export function formatSeconds(seconds: number): string {
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
  return `${Math.floor(seconds / 86400)}d ${Math.floor((seconds % 86400) / 3600)}h`;
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[unit]}`;
}

export function formatTime(milliseconds: number): string {
  if (!milliseconds) return "—";
  return new Date(milliseconds).toLocaleString("en-US", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// Base units of an asset without known decimals stay exact, grouped.
export function formatAmount(decimal: string): string {
  const negative = decimal.startsWith("-");
  const digits = negative ? decimal.slice(1) : decimal;
  return `${negative ? "−" : ""}${digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",")}`;
}

export function shortHex(value: string): string {
  return value.length > 14 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value;
}

const integerText = /^-?\d+$/;

// formatUnits shows an exact integer amount in token units: five significant
// digits at or above one, four below one, never in exponent notation.
export function formatUnits(raw: string, decimals: number): string {
  if (!integerText.test(raw)) return "—";
  const negative = raw.startsWith("-");
  const value = BigInt(negative ? raw.slice(1) : raw);
  const scale = 10n ** BigInt(decimals);
  const whole = value / scale;
  let places: number;
  if (whole > 0n) {
    places = Math.max(0, 5 - whole.toString().length);
  } else {
    const fraction = (value % scale).toString().padStart(decimals, "0");
    const firstDigit = fraction.search(/[1-9]/);
    places = firstDigit < 0 ? 0 : Math.min(decimals, firstDigit + 4);
  }
  places = Math.min(places, decimals);
  const step = 10n ** BigInt(decimals - places);
  const rounded = (value + step / 2n) / step;
  const text = rounded.toString().padStart(places + 1, "0");
  const integer = text.slice(0, text.length - places).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const decimalsText = places > 0 ? text.slice(text.length - places).replace(/0+$/, "") : "";
  const shown = decimalsText ? `${integer}.${decimalsText}` : integer;
  return negative && shown.replace(/[0.,]/g, "") !== "" ? `−${shown}` : shown;
}

// Native amounts arrive in wei. Sub-micro-ETH amounts read better in gwei.
export function formatEth(wei: string | number | undefined): string {
  if (wei === undefined) return "—";
  const raw = typeof wei === "number" ? (Number.isFinite(wei) ? BigInt(Math.round(wei)).toString() : "") : wei;
  if (!integerText.test(raw)) return "—";
  const magnitude = raw.replace("-", "");
  if (magnitude !== "0" && magnitude.length <= 12) return `${formatUnits(raw, 9)} gwei`;
  return `${formatUnits(raw, 18)} ETH`;
}

export function formatGwei(wei: string | number): string {
  const raw = typeof wei === "number" ? BigInt(Math.round(wei)).toString() : wei;
  return `${formatUnits(raw, 9)} gwei`;
}

export function formatMilliseconds(nanoseconds: number | undefined): string {
  return nanoseconds === undefined ? "—" : `${Math.round(nanoseconds / 1e6)} ms`;
}
