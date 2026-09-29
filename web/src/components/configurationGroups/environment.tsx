import { useState } from 'react';
import { Group, Rows, type GroupProps } from './fields';

// Values render masked; revealing is local to this browser and sends nothing. Removing a row masks
// every value again, since row positions shift.
export function EnvironmentGroup({ spec, set }: GroupProps) {
  const [revealed, setRevealed] = useState<ReadonlySet<number>>(new Set());
  const all = spec.env.length > 0 && spec.env.every((_, i) => revealed.has(i));
  const toggle = (i: number) => setRevealed((r) => { const next = new Set(r); if (!next.delete(i)) next.add(i); return next; });
  return <Group title="Environment">
    <div><button type="button" className="btn-secondary" aria-pressed={all} onClick={() => setRevealed(all ? new Set() : new Set(spec.env.map((_, i) => i)))}>{all ? 'Hide all' : 'Reveal all'}</button></div>
    <Rows noun="Variable" rows={spec.env} blank={{ name: '', value: '' }}
      onChange={(env) => { if (env.length < spec.env.length) setRevealed(new Set()); set({ env }); }}
      render={(e, update, label, i) => {
        const who = e.name || label.toLowerCase();
        return <>
          <input aria-label={`${label} name`} value={e.name} onChange={(ev) => update({ ...e, name: ev.target.value })} />
          <input aria-label={`Value of ${who}`} type={revealed.has(i) ? 'text' : 'password'} autoComplete="off" value={e.value} onChange={(ev) => update({ ...e, value: ev.target.value })} />
          <button type="button" className="btn-secondary" aria-pressed={revealed.has(i)} aria-label={`${revealed.has(i) ? 'Hide' : 'Reveal'} value of ${who}`} onClick={() => toggle(i)}>{revealed.has(i) ? 'Hide' : 'Reveal'}</button>
        </>;
      }} />
  </Group>;
}
