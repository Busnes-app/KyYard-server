import type { ReactNode } from 'react';
import type { ExplicitSpec } from '../../tenant';

// Every group edits the draft spec through set; the form's fieldset disables them when read-only.
export type GroupProps = { spec: ExplicitSpec; set: (patch: Partial<ExplicitSpec>) => void };

// num reads a numeric input; an empty or partial entry is 0 until the operator finishes typing.
export const num = (v: string) => { const n = Number(v); return Number.isFinite(n) ? n : 0; };
export const int = (v: string) => Math.trunc(num(v));

export function Group({ title, children }: { title: string; children: ReactNode }) {
  return <fieldset className="ky-config-group"><legend>{title}</legend>{children}</fieldset>;
}

export function Text({ label, value, onChange, placeholder }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string }) {
  return <label>{label}<input value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} /></label>;
}

export function Num({ label, value, onChange, step }: { label: string; value: number; onChange: (v: string) => void; step?: string }) {
  return <label>{label}<input type="number" step={step ?? '1'} value={value} onChange={(e) => onChange(e.target.value)} /></label>;
}

export function Check({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return <label className="ky-config-check"><input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />{label}</label>;
}

// Lines edits a list one entry per line; the split is lossless while typing and the form drops
// blank entries when it builds the spec.
export function Lines({ label, value, onChange }: { label: string; value: string[]; onChange: (v: string[]) => void }) {
  return <label>{label}<textarea rows={Math.min(Math.max(value.length, 2), 8)} value={value.join('\n')} onChange={(e) => onChange(e.target.value === '' ? [] : e.target.value.split('\n'))} /></label>;
}

// Rows is a list of editable rows with an add and a per-row remove button.
export function Rows<T>({ noun, rows, onChange, blank, render }: { noun: string; rows: T[]; onChange: (rows: T[]) => void; blank: T; render: (row: T, update: (row: T) => void, label: string, index: number) => ReactNode }) {
  return <div className="dr-stack">
    {rows.map((row, i) => <div className="ky-config-row" key={i}>
      {render(row, (next) => onChange(rows.map((r, j) => j === i ? next : r)), `${noun} ${i + 1}`, i)}
      <button type="button" className="btn-secondary" aria-label={`Remove ${noun.toLowerCase()} ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>Remove</button>
    </div>)}
    <div><button type="button" className="btn-secondary" onClick={() => onChange([...rows, blank])}>Add {noun.toLowerCase()}</button></div>
  </div>;
}
