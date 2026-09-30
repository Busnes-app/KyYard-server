import { useState } from 'react';
import { LiveTerminal, type Start } from './ContainerTerminal';
import { EmptyNotice } from './StateNotice';
import type { Pod } from '../tenant';

// PodTerminal opens a shell in one container of a running pod: the server holds the start to the
// route's pod and checks the UID the inventory reported, so a replaced pod is refused.
export function PodTerminal({ base, pod, scope }: { base: string; pod: Pod; scope: string }) {
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '');
  const [shell, setShell] = useState('/bin/sh');
  const [confirm, setConfirm] = useState('');
  const [start, setStart] = useState<Start | null>(null);
  const label = `${pod.namespace}/${pod.name}`;
  return <section aria-label={`Terminal for ${label}`} style={{ marginTop: 12, minWidth: 300 }}>
    <h3>Terminal · {label}</h3>
    <p>{scope}</p>
    <p>Organization administrators only. The shell runs as the container's own user. Closing disconnects the terminal; it does not guarantee that the process stops. Sessions end after 15 minutes without input or 8 hours total.</p>
    {!pod.uid ? <EmptyNotice>The inventory does not report this pod's identity yet. Refresh after the agent's next report, or upgrade the cluster agent.</EmptyNotice>
      : start ? <LiveTerminal key={label} path={`${base}/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}/exec`} start={start} label="Pod terminal" />
      : <form onSubmit={(event) => { event.preventDefault(); setStart({ spec: { pod: { namespace: pod.namespace, name: pod.name, container, uid: pod.uid }, argv: [shell] }, confirm }); }}>
        <label>Container <select value={container} onChange={(e) => setContainer(e.target.value)}>{pod.containers.map((c) => <option key={c.name} value={c.name}>{c.name}</option>)}</select></label>
        <label>Shell executable <input required maxLength={1024} value={shell} onChange={(e) => setShell(e.target.value)} /></label>
        <label>Confirm pod name <input required value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" /></label>
        <button className="btn-primary" disabled={confirm !== pod.name || !container || !shell.trim()}>Open terminal in {container || 'chosen container'}</button>
      </form>}
  </section>;
}
