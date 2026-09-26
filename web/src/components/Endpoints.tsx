import React, { useState } from 'react';
import { secureFetch } from '../api';
import { NAMESPACE_RULE, parseNamespaces, tenantWrite, useTenantResource, validNamespaces, type Endpoint, type EnrollmentToken } from '../tenant';
import { EmptyNotice, StateNotice } from './StateNotice';
import { Link } from './Link';
import { endpointPath } from '../router';

const terminal = (s: string) => s === 'revoked' || s === 'expired';
// healthBadge colours a cluster_health; anything else (unknown) is secondary.
export const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };

// Fixed texts for the refusals a cluster enrollment can meet.
const mintRefusals: Record<string, string> = {
  agent_image_unpinned: 'Set KY_AGENT_IMAGE to a digest-pinned image before enrolling a cluster.',
  https_required: 'Kubernetes enrollment needs KY_APP_URL on HTTPS.',
};

// Names come from whoever redeemed the token. The server refuses control characters and line
// separators; the dialog strips the same classes again (C0, DEL, C1 and Unicode line and
// paragraph separators) and clamps the length before the name shares a prompt with a fingerprint.
export const displayName = (name: string) => {
  const clean = name.replace(/[\p{Cc}\p{Zl}\p{Zp}]/gu, '');
  return clean.length > 64 ? clean.slice(0, 63) + '…' : clean;
};

// Endpoints live under their environment because enrollment tokens are minted per environment.
export const Endpoints: React.FC<{ org: string; env: string }> = ({ org, env }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const envBase = `${base}/environments/${encodeURIComponent(env)}`;
  const endpoints = useTenantResource<Endpoint[]>(`${envBase}/endpoints`);
  const [token, setToken] = useState<EnrollmentToken | null>(null);
  const [runtime, setRuntime] = useState<'docker' | 'kubernetes'>('docker');
  const [clusterName, setClusterName] = useState('');
  const [namespaces, setNamespaces] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const namespacesValid = validNamespaces(parseNamespaces(namespaces));

  const mint = async () => {
    setBusy(true);
    setMessage('');
    try {
      const listed = parseNamespaces(namespaces);
      const body = runtime === 'kubernetes' ? { runtime, name: clusterName.trim(), ...(listed.length ? { namespaces: listed } : {}) } : { runtime };
      const resp = await secureFetch(`${envBase}/enrollment-tokens`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!resp.ok) {
        const code = resp.status === 409 ? ((await resp.json().catch(() => ({}))) as { code?: string }).code ?? '' : '';
        setMessage(resp.status === 403 ? 'You do not have permission to enroll hosts here.' : mintRefusals[code] ?? (resp.status === 400 && runtime === 'kubernetes' ? 'Name the cluster in printable text, at most 255 bytes.' : `Could not create an enrollment token (${resp.status}).`));
        return;
      }
      setToken((await resp.json()) as EnrollmentToken);
    } catch {
      setMessage('Offline: the server could not be reached.');
    } finally {
      setBusy(false);
    }
  };
  const download = (t: EnrollmentToken) => {
    const url = URL.createObjectURL(new Blob([t.manifest ?? ''], { type: 'application/yaml' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = t.manifest_file ?? 'kyyard-agent.yaml';
    link.click();
    URL.revokeObjectURL(url);
  };
  const acknowledgeKey = async (e: Endpoint) => {
    if (!e.pending_fingerprint) return;
    if (!window.confirm(`"${displayName(e.name)}" offered a new key.\n\nCurrent:  ${e.fingerprint}\nPending:  ${e.pending_fingerprint}\n\nAcknowledge only if the host printed the pending fingerprint. The current key stops working immediately.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/keys/${encodeURIComponent(e.pending_fingerprint)}/acknowledge`, 'POST');
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };
  const clearAlert = async (e: Endpoint, id: number) => {
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/events/${id}/acknowledge`, 'POST');
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };
  const act = async (e: Endpoint, action: 'approve' | 'reject' | 'revoke') => {
    const name = displayName(e.name);
    const prompts = {
      approve: `Approve "${name}" with key fingerprint\n\n${e.fingerprint}\n\nOnly approve if this matches the fingerprint the host printed.`,
      reject: `Reject the pending enrollment of "${name}"? The host will have to enroll again.`,
      revoke: `Revoke "${name}"? Its identity stops working immediately and cannot be restored.`,
    };
    if (!window.confirm(prompts[action])) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/endpoints/${encodeURIComponent(e.id)}/${action}`, 'POST', action === 'approve' ? { fingerprint: e.fingerprint } : undefined);
    setBusy(false);
    setMessage(err);
    if (!err) endpoints.reload();
  };

  return (
    <section className="panel" aria-labelledby="endpoints-heading">
      <div className="panel-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
        <h2 id="endpoints-heading" style={{ fontSize: 16 }}>Endpoints</h2>
        <button className="btn-secondary" onClick={endpoints.reload}>Refresh hosts</button>
        <label>Runtime <select aria-label="Runtime" value={runtime} onChange={(e) => setRuntime(e.target.value === 'kubernetes' ? 'kubernetes' : 'docker')}>
          <option value="docker">Docker host</option>
          <option value="kubernetes">Kubernetes cluster</option>
        </select></label>
        {runtime === 'kubernetes' && <input aria-label="Cluster name" placeholder="Cluster name" required maxLength={255} value={clusterName} onChange={(e) => setClusterName(e.target.value)} />}
        {runtime === 'kubernetes' && <input aria-label="Namespaces to deploy to" placeholder="Namespaces to deploy to (optional)" aria-invalid={!namespacesValid} value={namespaces} onChange={(e) => setNamespaces(e.target.value)} />}
        <button disabled={busy || (runtime === 'kubernetes' && (clusterName.trim() === '' || !namespacesValid))} onClick={() => void mint()}>{runtime === 'kubernetes' ? 'Enroll a cluster' : 'Enroll a host'}</button>
      </div>
      {runtime === 'kubernetes' && !namespacesValid && <p role="alert">{NAMESPACE_RULE}</p>}
      {token?.manifest && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="Enrollment manifest">
          <p><strong>Shown once.</strong> Apply this manifest as cluster-admin before {new Date(token.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.command}</pre>
          <details><summary>Manifest</summary><pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.manifest}</pre></details>
          {token.note && <p>{token.note}</p>}
          <p>{token.disclosure}</p>
          <button onClick={() => download(token)}>Download manifest</button>{' '}
          <button className="btn-secondary" onClick={() => { void navigator.clipboard?.writeText(token.manifest ?? ''); }}>Copy manifest</button>{' '}
          <button className="btn-secondary" onClick={() => setToken(null)}>Dismiss</button>
        </div>
      )}
      {token && !token.manifest && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="Enrollment command">
          <p><strong>Shown once.</strong> {token.command ? 'Run this on the remote host before' : 'Source enrollment token expires at'} {new Date(token.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{token.command ?? token.token}</pre>
          {token.note && <p>{token.note}</p>}
          <p>After starting the agent, refresh hosts below and approve the matching fingerprint. Then open the host to see its containers.</p>
          <details><summary>Source installation</summary>
            <p>Source builds can pass this token to kyyard-agent on stdin with --server set to the server address. Remote hosts require HTTPS.</p>
            <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{token.token}</pre>
          </details>
          <p>{token.disclosure}</p>
          <button className="btn-secondary" onClick={() => { void navigator.clipboard?.writeText(token.command ?? token.token); }}>{token.command ? 'Copy command' : 'Copy token'}</button>
          <button className="btn-secondary" onClick={() => setToken(null)}>Dismiss</button>
        </div>
      )}
      <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
      {endpoints.state === 'ready' && endpoints.data && (endpoints.data.length === 0 ? <EmptyNotice>No endpoints yet. Enroll a host to begin.</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Name</th><th>State</th><th>Host</th><th>Fingerprint</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>
              {endpoints.data.map((e) => (
                <tr key={e.id}>
                  <td data-label="Name"><Link to={endpointPath(org, e.id)}>{displayName(e.name)}</Link> <span className="font-mono" style={{ fontSize: 11, color: 'var(--ink)' }}>{e.runtime}</span>{e.cluster_health && <> <span className={`badge ${healthBadge[e.cluster_health] ?? 'badge-secondary'}`}>{e.cluster_health}</span></>}</td>
                  <td data-label="State"><span className={`badge ${e.state === 'pending' ? 'badge-accent' : e.state === 'active' ? 'badge-success' : 'badge-danger'}`}>{e.state}</span></td>
                  <td data-label="Host">{e.runtime === 'kubernetes' ? `${e.facts.node_count ?? '?'} nodes` : e.facts.hostname ?? ''} <span style={{ fontSize: 11, color: 'var(--ink)' }}>{e.facts.runtime_version ?? e.facts.server_version ?? ''}</span></td>
                  <td data-label="Fingerprint" className="font-mono" style={{ fontSize: 11 }}>
                    <span title={e.fingerprint}>{e.fingerprint ? e.fingerprint.slice(0, 16) + '…' : '—'}</span>
                    {e.pending_fingerprint && <div><span className="badge badge-accent">rotation pending</span> <span title={e.pending_fingerprint}>{e.pending_fingerprint.slice(0, 16)}…</span></div>}
                    {e.alerts.filter((a) => a.kind !== 'rotation_pending').map((a) => (
                      <div key={a.id} role="alert"><span className="badge badge-danger">{a.kind}</span> {a.details}{' '}
                        <button className="btn-secondary" disabled={busy} onClick={() => void clearAlert(e, a.id)}>Clear</button>
                      </div>
                    ))}
                  </td>
                  <td data-label="Actions">
                    {e.state === 'pending' && <>
                      <button disabled={busy} onClick={() => void act(e, 'approve')}>Approve</button>{' '}
                      <button className="btn-secondary" disabled={busy} onClick={() => void act(e, 'reject')}>Reject</button>
                    </>}
                    {e.pending_fingerprint && <><button disabled={busy} onClick={() => void acknowledgeKey(e)}>Acknowledge rotation</button>{' '}</>}
                    {!terminal(e.state) && e.state !== 'pending' && <button className="btn-danger" disabled={busy} onClick={() => void act(e, 'revoke')}>Revoke</button>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
    </section>
  );
};
