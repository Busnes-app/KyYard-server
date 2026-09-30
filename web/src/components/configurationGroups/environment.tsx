import { useState } from 'react';
import { Group, Rows, type GroupProps } from './fields';

// A hidden value is not in the DOM at all; revealing is local to this browser and sends nothing.
// Removing a row hides every value again, since row positions shift; a new row opens for typing.
export function EnvironmentGroup({ spec, set }: GroupProps) {
  const [revealed, setRevealed] = useState<ReadonlySet<number>>(new Set());
  const all = spec.env.length > 0 && spec.env.every((_, i) => revealed.has(i));
  const toggle = (i: number) => setRevealed((r) => { const next = new Set(r); if (!next.delete(i)) next.add(i); return next; });
  return <Group title="Environment">
    <div><button type="button" className="btn-secondary" aria-pressed={all} onClick={() => setRevealed(all ? new Set() : new Set(spec.env.map((_, i) => i)))}>{all ? 'Hide all' : 'Reveal all'}</button></div>
    <Rows noun="Variable" rows={spec.env} blank={{ name: '', value: '' }}
      onChange={(env) => {
        if (env.length < spec.env.length) setRevealed(new Set());
        else if (env.length > spec.env.length) setRevealed((r) => new Set(r).add(spec.env.length));
        set({ env });
      }}
      render={(e, update, label, i) => {
        const who = e.name || label.toLowerCase();
        return <>
          <input aria-label={`${label} name`} placeholder="NAME" value={e.name} onChange={(ev) => update({ ...e, name: ev.target.value })} />
          {revealed.has(i) ? <>
            <textarea aria-label={`Value of ${who}`} placeholder="value" rows={1} autoComplete="off" data-1p-ignore data-lpignore="true" data-bwignore value={e.value} onChange={(ev) => update({ ...e, value: ev.target.value })} />
            <button type="button" className="btn-secondary" aria-label={`Hide value of ${who}`} onClick={() => toggle(i)}>Hide</button>
          </> : <button type="button" className="btn-secondary ky-config-masked" aria-label={`Reveal value of ${who}`} onClick={() => toggle(i)}>••••••</button>}
        </>;
      }} />
  </Group>;
}
