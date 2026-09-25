import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';
import { knownBlockers } from './ApplicationPreflight';

export type ApplicationInstance = { id: string; application_id: string; endpoint_id: string; endpoint_name: string; project: string; revision: number; current_revision: number; previous_revision: number; mapping_version: number; container_count: number; containers: AdoptedContainer[] };
type AdoptedContainer = { id: string; name: string; image_id: string; created_at: string };
const REMOVAL_CODES: Record<string, string> = {
  endpoint_offline: 'The host is not connected.',
  deployment_planned: 'A deployment is planned or running for this instance; wait or let the plan expire.',
  deployment_in_progress: 'A deployment is planned or running for this instance; wait or let the plan expire.',
  deployment_not_sent: 'The removal could not be sent to the host.',
  adoption_changed: 'Host inventory is stale or changed. Refresh applications and try again.',
  removal_too_large: 'This instance has more containers than KyYard removes in one operation; release it and remove the containers by hand.',
};
const REMOVAL_BLOCKERS = {
  unadopted_project_containers: 'The host reported containers of this project that are not adopted. Inventory can be up to a minute stale after a deployment: retry shortly; if they remain, adopt or remove them by hand.',
  apply_outcome_unknown: "The last apply's outcome is unknown; inspect the host before removing.",
};
const REMOVAL_SENT = 'Removal sent; watch Deployment history for progress.';
const REMOVAL_REFUSED = 'Removal refused. Refresh applications and check your access.';
const OUTCOME_UNKNOWN = 'Outcome unknown. Refresh applications before continuing.';
type Preview = { application_name: string; endpoint_name: string; endpoint_id: string; project: string; revision: number; digest: string; containers: AdoptedContainer[] };

export function ApplicationAdoption({ base, org, env, applicationName, instance, onChanged }: { base: string; org: string; env: string; applicationName: string; instance?: ApplicationInstance; onChanged: (status?: string) => void }) {
  const [endpoint, setEndpoint] = useState('');
  const [offset, setOffset] = useState(0);
  const hosts = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/endpoints?offset=${offset}&limit=20`);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [uncertain, setUncertain] = useState(false);
  const release = async () => {
    if (!instance || !window.confirm(`Release "${applicationName}" ownership of project "${instance.project}" on host "${instance.endpoint_name}" (${instance.endpoint_id})? This only removes the association; containers keep running.`)) return;
    setBusy(true);
    try {
      const r = await secureFetch(`${base}/adoption`, { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: instance.id, confirm: instance.project }) });
      if (r.ok) { onChanged(); return; }
      setUncertain(r.status >= 500);
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const code = payload && typeof payload === 'object' && 'code' in payload ? (payload as { code?: unknown }).code : undefined;
        setMessage(code === 'deployment_planned' ? 'A deployment plan for this instance is still valid. Plans expire ten minutes after they are made; release after that.' : 'Release refused. Refresh applications and check your access.');
        return;
      }
      setMessage(r.status >= 500 ? 'Outcome unknown. Refresh applications before continuing.' : 'Release refused. Refresh applications and check your access.');
    } catch { setUncertain(true); setMessage('Outcome unknown. Refresh applications before continuing.'); }
    finally { setBusy(false); }
  };
  if (instance) return <div>
    <p>Adopted project <bdi>{instance.project}</bdi> · host {instance.endpoint_name} · {instance.container_count} recorded containers. Adoption does not mean the running configuration matches revision {instance.revision}.</p>
    <button className="btn-secondary" disabled={busy || uncertain} onClick={() => void release()}>Release adoption</button>
    {message && <p role="status">{message}</p>}
    <RemoveApplication base={base} instance={instance} onChanged={onChanged} />
  </div>;
  return <div className="dr-stack">
    <h3>Adopt an existing Compose project</h3>
    <p>Associate this application with an exact container snapshot. No restart, relabeling or deployment. Networks and volumes remain unowned.</p>
    <StateNotice state={hosts.state} onRetry={hosts.reload} />
    {hosts.state === 'ready' && <label>Docker host<select value={endpoint} onChange={(e) => setEndpoint(e.target.value)}><option value="">Choose a host</option>{hosts.data?.map((h) => <option key={h.id} value={h.id}>{h.name} · {h.state}</option>)}</select></label>}
    {(offset > 0 || (hosts.data?.length ?? 0) === 20) && <div className="ky-pagination"><button className="btn-secondary" disabled={offset === 0} onClick={() => { setEndpoint(''); setOffset(offset - 20); }}>Previous hosts</button><button className="btn-secondary" disabled={hosts.state !== 'ready' || (hosts.data?.length ?? 0) < 20} onClick={() => { setEndpoint(''); setOffset(offset + 20); }}>Next hosts</button></div>}
    {endpoint && <ProjectChoice key={endpoint} base={base} org={org} endpoint={endpoint} onChanged={onChanged} />}
  </div>;
}
function ProjectChoice({ base, org, endpoint, onChanged }: { base: string; org: string; endpoint: string; onChanged: () => void }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}/inventory`);
  const [project, setProject] = useState('');
  const projects = [...new Set(inventory.data?.snapshot.containers.map((c) => c.compose_project).filter((p): p is string => Boolean(p)) ?? [])].sort();
  return <>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && <label>Compose project<select value={project} onChange={(e) => setProject(e.target.value)}><option value="">Choose a project</option>{projects.map((p) => <option key={p} value={p}>{p}</option>)}</select></label>}
    {inventory.state === 'ready' && projects.length === 0 && <p>No Compose projects reported by this host.</p>}
    {project && <ConfirmAdoption key={project} base={base} endpoint={endpoint} project={project} onChanged={onChanged} />}
  </>;
}
function ConfirmAdoption({ base, endpoint, project, onChanged }: { base: string; endpoint: string; project: string; onChanged: () => void }) {
  const preview = useTenantResource<Preview>(`${base}/adoption?endpoint=${encodeURIComponent(endpoint)}&project=${encodeURIComponent(project)}`);
  const pagination = usePagination(preview.data?.containers ?? [], preview.data?.digest ?? '');
  const [confirm, setConfirm] = useState('');
  const [blocked, setBlocked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const adopt = async () => {
    if (!preview.data) return;
    setBusy(true);
    try {
      const r = await secureFetch(`${base}/adoption`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ endpoint_id: endpoint, project, digest: preview.data.digest, confirm }) });
      if (r.ok) { onChanged(); return; }
      setBlocked(true);
      setMessage(r.status >= 500 ? 'Outcome unknown. Refresh applications before continuing.' : 'Adoption refused. Refresh applications and preview again; check permissions and fresh, complete inventory.');
    } catch { setBlocked(true); setMessage('Outcome unknown. Refresh applications before continuing.'); }
    finally { setBusy(false); }
  };
  return <>
    <StateNotice state={preview.state} onRetry={preview.reload} />
    {preview.state === 'error' && <p>Adoption needs an active host and a complete container report from the last three minutes.</p>}
    {preview.state === 'ready' && preview.data && <>
      <p>Associate <strong>{preview.data.application_name}</strong> (revision {preview.data.revision}) with <strong>{preview.data.project}</strong> on <strong>{preview.data.endpoint_name}</strong>. Review the exact container list ({preview.data.containers.length}):</p>
      {pagination.controls}
      <ul className="ky-list">{pagination.rows.map((c) => <li key={c.id} style={{ overflowWrap: 'anywhere' }}><strong>{c.name}</strong><br />ID: {c.id}<br />Image: {c.image_id}<br />Created: {new Date(c.created_at).toLocaleString()}</li>)}</ul>
      <label>Type the project name to confirm<input value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" disabled={busy || blocked} /></label>
      <button disabled={busy || blocked || confirm !== preview.data.project} onClick={() => void adopt()}>Adopt reviewed containers</button>
    </>}
    {message && <p role="status">{message}</p>}
  </>;
}
function RemoveApplication({ base, instance, onChanged }: { base: string; instance: ApplicationInstance; onChanged: (status?: string) => void }) {
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [message, setMessage] = useState('');
  const remove = async () => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(`${base}/removal`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: instance.id, confirm }) });
      if (r.status === 202) { onChanged(REMOVAL_SENT); return; }
      setConfirm('');
      if (r.status === 403) { setMessage('Only an administrator can remove applications.'); return; }
      if (r.status === 501) { setMessage('Upgrade the host agent to enable application removal.'); return; }
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const code = payload && typeof payload === 'object' && 'code' in payload ? (payload as { code?: unknown }).code : undefined;
        const blockers = knownBlockers(payload, REMOVAL_BLOCKERS);
        setMessage(blockers.length ? blockers.map(b => REMOVAL_BLOCKERS[b]).join(' ') : (typeof code === 'string' && REMOVAL_CODES[code]) || REMOVAL_REFUSED);
        return;
      }
      setUncertain(r.status >= 500);
      setMessage(r.status >= 500 ? OUTCOME_UNKNOWN : REMOVAL_REFUSED);
    } catch { setUncertain(true); setMessage(OUTCOME_UNKNOWN); }
    finally { setBusy(false); }
  };
  return <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void remove(); }}>
    <h3>Remove application</h3>
    <p>Stops and removes the {instance.container_count} adopted containers of <bdi>{instance.project}</bdi> on {instance.endpoint_name}. Named volumes, images and the saved revisions are kept; the application is marked removed and can be discarded later. Nothing rolls back.</p>
    <label>Confirm removal project<input value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={busy || uncertain} autoComplete="off" /></label>
    <button className="btn-danger" disabled={busy || uncertain || confirm !== instance.project}>Remove application</button>
    {message && <p role="alert">{message}</p>}
  </form>;
}
