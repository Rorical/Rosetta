import type { ReactNode } from "react";
import { shortHex } from "../format";

export type Tone = "good" | "warning" | "serious" | "critical" | "neutral";

export function Logo({ size }: { size: number }) {
  return (
    <picture>
      <source srcSet="/icon.svg" media="(prefers-color-scheme: dark)" />
      <img src="/icon-white.svg" alt="" width={size} height={size} />
    </picture>
  );
}

export function StatusDot({ tone, label }: { tone: Tone; label: string }) {
  return <span className={`dot ${tone}`} role="img" aria-label={label} />;
}

export function Status({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={`status ${tone}`}>{children}</span>;
}

export function Group({ title, trailing, children }: { title?: string; trailing?: ReactNode; children: ReactNode }) {
  return (
    <section className="group">
      {(title || trailing) && (
        <header>
          {title && <h2>{title}</h2>}
          {trailing}
        </header>
      )}
      <div className="group-body">{children}</div>
    </section>
  );
}

export function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="row">
      <span className="row-label">{label}</span>
      <span className="row-value">{children}</span>
    </div>
  );
}

export function Hex({ value }: { value: string }) {
  return (
    <code className="hex" title={value}>
      {shortHex(value)}
    </code>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="empty">{children}</p>;
}

export function Segmented<T extends string>({ label, options, value, onChange }: { label: string; options: { value: T; label: string }[]; value: T; onChange: (value: T) => void }) {
  return (
    <div className="segmented" role="radiogroup" aria-label={label}>
      {options.map((option) => (
        <button key={option.value} role="radio" aria-checked={option.value === value} onClick={() => onChange(option.value)}>
          {option.label}
        </button>
      ))}
    </div>
  );
}
