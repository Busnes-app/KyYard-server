import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';
import { messages } from './ApplicationPreflight';

type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[] };
type Deployment = { id: string; instance_id: string; endpoint_id: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; plan: { project: string; services: PlannedService[] } };
type Props = { base: string; instanceID: string; mappingVersion: number; revision: number; project: string };

export function ApplicationDeploymentPlan(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close deployment plan' : 'Deployment plan'}</button>
    {open && <PlanView key={`${props.instanceID}/${props.mappingVersion}/${props.revision}`} {...props} />}
  </section>;
}
function PlanView({ base, instanceID, mappingVersion, revision, project }: Props) {
  const resource = useTenantResource<Deployment[]>(`${base}/deployments`);
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [error, setError] = useState<string[]>([]);
  const [planned, setPlanned] = useState<Deployment | null>(null);
  const current = planned ?? resource.data?.find(d => d.instance_id === instanceID) ?? null;
  const page = usePagination(current?.plan.services ?? [], base);
  const plan = async () => {
    setBusy(true); setError([]);
    try {
      const r = await secureFetch(`${base}/deployments`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: instanceID, mapping_version: mappingVersion, revision, confirm }) });
      if (r.ok) {
        const body: unknown = await r.json().catch(() => null);
        if (body && typeof body === 'object' && 'instance_id' in body) setPlanned(body as Deployment);
        setConfirm('');
        return;
      }
      setBlocked(true);
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const blockers = payload && typeof payload === 'object' && Array.isArray((payload as { blockers?: unknown }).blockers) ? ((payload as { blockers: unknown[] }).blockers.filter((b): b is keyof typeof messages => typeof b === 'string' && b in messages)) : [];
        setError(blockers.length ? blockers.map(b => messages[b]) : ['Ownership, mapping or the definition changed. Refresh applications and review before planning again.']);
        return;
      }
      setError([r.status === 403 ? 'You do not have permission to plan deployments.' : 'The plan was refused or its outcome is unknown. Refresh before trying again.']);
    } catch { setBlocked(true); setError(['The outcome is unknown. Refresh before trying again.']); }
    finally { setBusy(false); }
  };
  return <>
    <p>A plan records the exact revision, mapping and image identities a deployment would use. It is not executed here: no containers change, no images are pulled and no secret values are read. Runtime configuration remains unverified.</p>
    <StateNotice state={resource.state} onRetry={resource.reload} />
    {resource.state === 'ready' && (current ? <>
      <p>Plan for revision {current.revision}, mapping version {current.mapping_version}, project <bdi>{current.plan.project}</bdi>. {current.expired ? 'This plan has expired; plan again to continue.' : `Expires ${new Date(current.expires_at).toLocaleString()}.`} It remains inert on its own; only planning again replaces it.</p>
      {page.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Pinned image</th><th>Replaces container</th><th>Secrets</th></tr></thead><tbody>{page.rows.map(s => <tr key={s.name}>
        <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
        <td data-label="Pinned image"><div className="ky-resource-name"><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></div></td>
        <td data-label="Replaces container"><div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div></td>
        <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
      </tr>)}</tbody></table>
    </> : <p>No plan for this instance.</p>)}
    {resource.state === 'ready' && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void plan(); }}>
      <p>Planning replaces any earlier plan for this instance. Type the project name <bdi>{project}</bdi> to confirm. Nothing runs.</p>
      <label>Confirm plan project<input value={confirm} onChange={e => setConfirm(e.target.value)} disabled={busy || blocked} autoComplete="off" /></label>
      <button disabled={busy || blocked || confirm !== project}>Plan deployment</button>
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => { setBlocked(false); setError([]); setPlanned(null); resource.reload(); }}>Refresh plan</button>
      {error.length > 0 && <ul role="alert">{error.map(e => <li key={e}>{e}</li>)}</ul>}
    </form>}
  </>;
}
