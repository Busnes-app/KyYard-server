import { useState } from 'react';
import { Group, Rows, type GroupProps } from './fields';

export function EnvironmentGroup({ spec, set }: GroupProps) {
  return <Group title="Environment"><EnvRows noun="Variable" env={spec.env} blank={{ name: '', value: '' }} onChange={(env) => set({ env })} /></Group>;
}

type Env = { name: string; value?: string; secret_ref?: string; config_map_ref?: string };

// A hidden value is not in the DOM at all; revealing is local to this browser and sends nothing.
// Removing a row hides every value again, since row positions shift; a new row opens for typing.
// A Secret or ConfigMap reference is shown by name and cannot be edited or removed here.
export function EnvRows<T extends Env>({ noun, env, blank, onChange }: { noun: string; env: T[]; blank: T; onChange: (env: T[]) => void }) {
  const [revealed, setRevealed] = useState<ReadonlySet<number>>(new Set());
  const literal = (e: Env) => !e.secret_ref && !e.config_map_ref;
  const literals = env.flatMap((e, i) => literal(e) ? [i] : []);
  const all = literals.length > 0 && literals.every((i) => revealed.has(i));
  const toggle = (i: number) => setRevealed((r) => { const next = new Set(r); if (!next.delete(i)) next.add(i); return next; });
  return <>
    <div><button type="button" className="btn-secondary" aria-pressed={all} onClick={() => setRevealed(all ? new Set() : new Set(literals))}>{all ? 'Hide all' : 'Reveal all'}</button></div>
    <Rows noun={noun} rows={env} blank={blank} locked={(e) => !literal(e)}
      onChange={(next) => {
        if (next.length < env.length) setRevealed(new Set());
        else if (next.length > env.length) setRevealed((r) => new Set(r).add(env.length));
        onChange(next);
      }}
      render={(e, update, label, i) => {
        if (!literal(e)) return <><span>{e.name}</span><span>{e.secret_ref ? `Secret ${e.secret_ref}` : `ConfigMap ${e.config_map_ref}`}</span></>;
        const who = e.name || label.toLowerCase();
        return <>
          <input aria-label={`${label} name`} placeholder="NAME" value={e.name} onChange={(ev) => update({ ...e, name: ev.target.value })} />
          {revealed.has(i) ? <>
            <textarea aria-label={`Value of ${who}`} placeholder="value" rows={1} autoComplete="off" data-1p-ignore data-lpignore="true" data-bwignore value={e.value ?? ''} onChange={(ev) => update({ ...e, value: ev.target.value })} />
            <button type="button" className="btn-secondary" aria-label={`Hide value of ${who}`} onClick={() => toggle(i)}>Hide</button>
          </> : <button type="button" className="btn-secondary ky-config-masked" aria-label={`Reveal value of ${who}`} onClick={() => toggle(i)}>••••••</button>}
        </>;
      }} />
  </>;
}
