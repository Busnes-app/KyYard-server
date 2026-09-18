import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';

export type ApplicationInstance = { id: string; application_id: string; endpoint_id: string; endpoint_name: string; project: string; revision: number; container_count: number; containers: AdoptedContainer[] };
type AdoptedContainer = { id: string; name: string; image_id: string; created_at: string };
type Preview = { application_name: string; endpoint_name: string; endpoint_id: string; project: string; revision: number; digest: string; containers: AdoptedContainer[] };

export function ApplicationAdoption({ base, org, env, applicationName, instance, onChanged }: { base: string; org: string; env: string; applicationName: string; instance?: ApplicationInstance; onChanged: () => void }) {
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
      setMessage(r.status >= 500 ? 'Outcome unknown. Refresh applications before continuing.' : 'Release refused. Refresh applications and check your access.');
    } catch { setUncertain(true); setMessage('Outcome unknown. Refresh applications before continuing.'); }
    finally { setBusy(false); }
  };
  if (instance) return <div>
    <p>Adopted project <bdi>{instance.project}</bdi> · host {instance.endpoint_name} · {instance.container_count} recorded containers. Adoption does not mean the running configuration matches revision {instance.revision}.</p>
    <button className="btn-secondary" disabled={busy || uncertain} onClick={() => void release()}>Release adoption</button>
    {message && <p role="status">{message}</p>}
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
