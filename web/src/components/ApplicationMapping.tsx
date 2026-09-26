import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';

type Mapping = { instance_id: string; version: number; mapped_revision: number; services: string[]; bindings: Record<string, string>; preview: { revision: number; digest: string; project: string; endpoint_name: string; containers: { id: string; name: string; image_id: string }[] } };
export function ApplicationMapping({ base, instanceID }: { base: string; instanceID: string }) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? 'Close service mapping' : 'Map services to containers'}</button>
    {open && <MappingView base={base} instanceID={instanceID} />}
  </section>;
}
function MappingView({ base, instanceID }: { base: string; instanceID: string }) {
  const resource = useTenantResource<Mapping>(`${base}/mapping`);
  const [message, setMessage] = useState('');
  return <>
    <p>Map each saved service to at most one adopted container. Unmapped services and containers stay unassigned. No containers are changed. All adopted containers must still be present in a fresh, complete host inventory.</p>
    <StateNotice state={resource.state} onRetry={resource.reload} />
    {message && <p role="status">{message}</p>}
    {resource.state === 'ready' && resource.data && (resource.data.instance_id === instanceID ? <MappingForm key={`${resource.data.version}/${resource.data.preview.digest}`} data={resource.data} base={base} onRefresh={() => { setMessage(''); resource.reload(); }} onSaved={() => { setMessage('Service mapping saved. Containers were not changed.'); resource.reload(); }} /> : <p role="alert">Adoption changed. Refresh applications before mapping.</p>)}
  </>;
}
function MappingForm({ data, base, onRefresh, onSaved }: { data: Mapping; base: string; onRefresh: () => void; onSaved: () => void }) {
  const [bindings, setBindings] = useState<Record<string, string>>(() => Object.fromEntries(data.services.filter(s => data.bindings[s]).map(s => [s, data.bindings[s]])));
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [error, setError] = useState('');
  const services = usePagination(data.services, base);
  const save = async () => {
    setBusy(true); setError('');
    try {
      const r = await secureFetch(`${base}/mapping`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: data.instance_id, version: data.version, digest: data.preview.digest, confirm, bindings }) });
      if (r.ok) { onSaved(); return; }
      setBlocked(true); setError(r.status === 403 ? 'Only an administrator can save service mappings.' : 'Mapping refused or its outcome is unknown. Refresh the mapping and host inventory before trying again.');
    } catch { setBlocked(true); setError('The outcome is unknown. Refresh the mapping and check the saved assignments before trying again.'); }
    finally { setBusy(false); }
  };
  return <form className="dr-stack" onSubmit={e => { e.preventDefault(); void save(); }}>
    <p>{data.preview.endpoint_name} · project <bdi>{data.preview.project}</bdi> · definition revision {data.preview.revision}</p>
    <p>{data.version === 0 ? 'No confirmed mapping yet.' : `Saved mapping version ${data.version} for revision ${data.mapped_revision}.`}</p>
    {data.version > 0 && data.mapped_revision !== data.preview.revision && <p role="alert">The definition changed. Review and save the mapping again. Assignments for removed services will be cleared.</p>}
    {services.controls}
    {services.rows.map(service => <label key={service}>{service}<select disabled={busy || blocked} value={bindings[service] ?? ''} onChange={e => setBindings(old => { const next = { ...old }; if (e.target.value) next[service] = e.target.value; else delete next[service]; return next; })}>
      <option value="">Unmapped</option>
      {data.preview.containers.map(c => <option key={c.id} value={c.id} disabled={Object.entries(bindings).some(([s,id]) => s !== service && id === c.id)}>{c.name} · {c.id}</option>)}
    </select>{bindings[service] && <small>Container {bindings[service]} · image {data.preview.containers.find(c => c.id === bindings[service])?.image_id}</small>}</label>)}
    <p>Saving replaces all assignments for this application. Type the project name <bdi>{data.preview.project}</bdi> to confirm.</p>
    <label>Confirm mapping project<input value={confirm} onChange={e => setConfirm(e.target.value)} disabled={busy || blocked} autoComplete="off" /></label>
    <button disabled={busy || blocked || confirm !== data.preview.project}>Save service mapping</button>
    <button type="button" className="btn-secondary" disabled={busy} onClick={onRefresh}>Refresh mapping</button>
    {error && <p role="alert">{error}</p>}
  </form>;
}
