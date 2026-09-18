import { useState } from 'react';
import { usePagination } from './Pagination';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';

type Draft = { id: string; name: string; latest_revision: number };
type Revision = { digest: string; spec: { services: { name: string; image: string; restart?: string; ports?: { target: number; published: number; host_ip?: string; protocol: string }[]; environment?: Record<string, { secret_ref: string }> }[] } };

function RevisionView({ base, draft }: { base: string; draft: Draft }) {
  const revision = useTenantResource<Revision>(`${base}/${encodeURIComponent(draft.id)}/revisions/${draft.latest_revision}`);
  return <div>
    <StateNotice state={revision.state} onRetry={revision.reload} />
    {revision.state === 'ready' && revision.data && <>
      <p style={{ overflowWrap: 'anywhere' }}>Revision {draft.latest_revision} · {revision.data.digest}</p>
      <ul>{revision.data.spec.services.map((service) => <li key={service.name}>
        <strong>{service.name}</strong> · {service.image} · restart: {service.restart || 'no'}
        {service.ports?.map((port, i) => <div key={i}>Port {port.host_ip || 'all interfaces'}:{port.published} → {port.target}/{port.protocol}</div>)}
        {Object.keys(service.environment ?? {}).length > 0 && <div>Encrypted environment keys: {Object.keys(service.environment ?? {}).join(', ')}</div>}
      </li>)}</ul>
    </>}
  </div>;
}

export function Applications({ org, env }: { org: string; env: string }) {
  const base = `/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/applications`;
  // Admission caps the organization at 100, so one bounded page covers this environment.
  const drafts = useTenantResource<Draft[]>(`${base}?limit=100`);
  const pagination = usePagination(drafts.data ?? [], base);
  const [name, setName] = useState('');
  const [source, setSource] = useState('');
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [message, setMessage] = useState('');
  const [selected, setSelected] = useState('');

  const write = async (method: string, path: string, body: unknown) => {
    setBusy(true); setMessage('');
    try {
      const response = await secureFetch(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (response.ok) { setSource(''); setName(''); setSelected(''); drafts.reload(); setMessage(method === 'POST' ? 'Draft imported. No containers were changed.' : 'Draft discarded.'); return; }
      if (response.status === 400) {
        const payload: unknown = await response.json();
        // Only display location, not arbitrary server text that might contain input.
        if (typeof payload === 'object' && payload !== null && 'diagnostic' in payload && typeof payload.diagnostic === 'object' && payload.diagnostic !== null && 'line' in payload.diagnostic && typeof payload.diagnostic.line === 'number') {
          setMessage(`Compose import refused near line ${payload.diagnostic.line}. Check the supported fields, explicit string values, and YAML syntax.`); return;
        }
        setMessage('Invalid import. Check the name, file size, and environment values.');
      } else if (response.status === 403) setMessage('An administrator must import or discard drafts.');
      else if (response.status === 409) setMessage('Name, revision, or storage conflict. Refresh drafts before continuing.');
      else if (response.status >= 500) { setUncertain(true); setMessage('The outcome is unknown. Refresh drafts and check whether the change completed before trying again.'); }
      else setMessage(`Request failed (${response.status}).`);
    } catch { setUncertain(true); setMessage('The outcome is unknown. Refresh drafts and check whether the change completed before trying again.'); }
    finally { setBusy(false); }
  };
  return <section className="panel" aria-label="Applications">
    <div className="panel-header"><h2 style={{ fontSize: 16 }}>Applications</h2></div>
    <p>Saved drafts are not deployed or linked to existing containers. Importing changes no running workloads.</p>
    <button type="button" className="btn-secondary" disabled={busy} onClick={drafts.reload}>Refresh drafts</button>
    {uncertain && <button type="button" disabled={busy || drafts.state !== 'ready'} onClick={() => setUncertain(false)}>I checked the drafts; enable changes</button>}
    <StateNotice state={drafts.state} onRetry={drafts.reload} />
    {pagination.controls}
    {drafts.state === 'ready' && <ul className="ky-list">
      {drafts.data?.length === 0 && <li>No saved applications in this environment.</li>}
      {pagination.rows.map((draft) => <li key={draft.id}>
        <strong>{draft.name}</strong> · Draft · Revision {draft.latest_revision}
        <div className="ky-inline-form">
          <button type="button" className="btn-secondary" onClick={() => setSelected(selected === draft.id ? '' : draft.id)}>View configuration for {draft.name}</button>
          <button type="button" className="btn-danger" disabled={busy || uncertain} onClick={() => {
            if (window.confirm(`Discard draft "${draft.name}" and all its saved revisions? Running containers are unchanged.`)) void write('DELETE', `${base}/${encodeURIComponent(draft.id)}`, { expected_revision: draft.latest_revision });
          }}>Discard {draft.name}</button>
        </div>
        {selected === draft.id && <RevisionView base={base} draft={draft} />}
      </li>)}
    </ul>}
    <details><summary>Import Compose draft</summary>
      <p>Initial supported fields: services with image, environment (explicit quoted strings), restart, and ports (long syntax with target/published, optional host_ip/protocol). Other fields are rejected, including volumes, build, env_file, command, and networks.</p>
      <p>Supply resolved values; variables, YAML anchors and aliases are unsupported. Escape literal dollars as $$. All environment values are encrypted and hidden from saved configuration views.</p>
      <form className="dr-stack" onSubmit={(event) => { event.preventDefault(); void write('POST', base, { name, compose: source }); }}>
        <label htmlFor="application-name">Application name</label>
        <input id="application-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={255} autoComplete="off" disabled={busy} />
        <label htmlFor="application-compose">Compose YAML (up to 64 KiB)</label>
        <textarea id="application-compose" value={source} onChange={(e) => setSource(e.target.value)} required maxLength={65536} rows={12} autoComplete="off" spellCheck={false} disabled={busy} style={{ width: '100%', fontFamily: 'monospace' }} />
        <button type="submit" disabled={busy || uncertain || !name.trim() || !source.trim()}>Import draft</button>
        <button type="button" className="btn-secondary" disabled={busy} onClick={() => { setSource(''); setName(''); }}>Clear input</button>
      </form>
    </details>
    {message && <p role="status">{message}</p>}
  </section>;
}
