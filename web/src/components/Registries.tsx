import React, { useState } from 'react';
import { secureFetch } from '../api';
import { EmptyNotice, StateNotice } from './StateNotice';
import { useTenantResource } from '../tenant';

interface Registry { id: string; host: string; name: string; username: string; has_credential: boolean; allow_private: boolean }
interface RegistryPolicy { anonymous_pull_enabled: boolean }

// Response bodies are never shown: every outcome maps to fixed text.
async function write(url: string, method: string, body?: unknown): Promise<string> {
  try {
    const resp = await secureFetch(url, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    if (resp.ok) return '';
    if (resp.status === 403) return 'Only an organization administrator can manage registries.';
    if (resp.status === 400) return 'Check the host, name and username, then try again.';
    if (resp.status === 404) return 'That registry no longer exists in this access scope.';
    return `Request failed (${resp.status}).`;
  } catch {
    return 'Offline: the server could not be reached.';
  }
}

const emptyForm = { host: '', name: '', username: '', allowPrivate: false };

export const Registries: React.FC<{ org: string }> = ({ org }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const registries = useTenantResource<Registry[]>(`${base}/registries`);
  const policy = useTenantResource<RegistryPolicy>(`${base}/registry-policy`);
  const [form, setForm] = useState(emptyForm);
  // The credential is write-only: never prefilled, dropped after every submit.
  const [credential, setCredential] = useState('');
  const [clear, setClear] = useState(false);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const run = async (url: string, method: string, body?: unknown) => {
    setBusy(true);
    const err = await write(url, method, body);
    setBusy(false);
    setMessage(err);
    return !err;
  };

  const save = async () => {
    const body: Record<string, unknown> = { host: form.host.trim(), name: form.name.trim(), username: form.username.trim(), allow_private: form.allowPrivate };
    // An absent key keeps the stored credential; "" removes it.
    if (clear) body.credential = '';
    else if (credential) body.credential = credential;
    setCredential('');
    setClear(false);
    if (await run(`${base}/registries`, 'PUT', body)) { setForm(emptyForm); registries.reload(); }
  };
  const edit = (r: Registry) => {
    setForm({ host: r.host, name: r.name, username: r.username, allowPrivate: r.allow_private });
    setCredential('');
    setClear(false);
    setMessage('');
  };
  const remove = async (r: Registry) => {
    if (!window.confirm(`Delete the registry entry for ${r.host} and its stored credential?`)) return;
    if (await run(`${base}/registries/${encodeURIComponent(r.id)}`, 'DELETE')) registries.reload();
  };
  const setAnonymous = async (enabled: boolean) => {
    if (await run(`${base}/registry-policy`, 'PUT', { anonymous_pull_enabled: enabled })) policy.reload();
  };

  return (
    <section className="panel" aria-labelledby="registries-heading">
      <h2 id="registries-heading" style={{ fontSize: 18 }}>Registries</h2>
      <StateNotice state={registries.state} onRetry={registries.reload} />
      {registries.state === 'ready' && registries.data && (registries.data.length === 0 ? <EmptyNotice>No registries. Images from hosts without an entry pull only when anonymous pulls are on.</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Host</th><th>Name</th><th>Username</th><th>Credential</th><th>Private addresses</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>
              {registries.data.map((r) => (
                <tr key={r.id}>
                  <td data-label="Host" className="font-mono">{r.host}</td>
                  <td data-label="Name">{r.name}</td>
                  <td data-label="Username">{r.username}</td>
                  <td data-label="Credential">{r.has_credential ? 'credential set' : 'no credential'}</td>
                  <td data-label="Private addresses">{r.allow_private ? 'private addresses allowed' : 'public only'}</td>
                  <td data-label="Actions" style={{ display: 'flex', gap: 8 }}>
                    <button className="btn-secondary" disabled={busy} onClick={() => edit(r)} aria-label={`Edit ${r.host}`}>Edit</button>
                    <button className="btn-danger" disabled={busy} onClick={() => void remove(r)} aria-label={`Delete ${r.host}`}>Delete</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      {registries.state === 'ready' && (
        <form onSubmit={(e) => { e.preventDefault(); void save(); }} className="ky-inline-form" aria-label="Add or update a registry">
          <label htmlFor="registry-host">Host</label>
          <input id="registry-host" value={form.host} onChange={(e) => setForm({ ...form, host: e.target.value })} required maxLength={261} placeholder="ghcr.io" />
          <label htmlFor="registry-name">Name</label>
          <input id="registry-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} required maxLength={64} />
          <label htmlFor="registry-username">Username</label>
          <input id="registry-username" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} maxLength={255} autoComplete="off" />
          <label htmlFor="registry-credential">Credential</label>
          <input id="registry-credential" type="password" autoComplete="new-password" value={credential} disabled={clear} onChange={(e) => setCredential(e.target.value)} maxLength={4096} aria-describedby="registry-credential-help" />
          <p id="registry-credential-help" style={{ flexBasis: '100%', margin: 0, color: 'var(--ink)', fontSize: 13 }}>Leave empty to keep the stored credential; use Clear to remove it.</p>
          <label><input type="checkbox" checked={clear} onChange={(e) => { setClear(e.target.checked); setCredential(''); }} />Clear credential</label>
          <label><input type="checkbox" checked={form.allowPrivate} onChange={(e) => setForm({ ...form, allowPrivate: e.target.checked })} />Allow private addresses</label>
          <button type="submit" disabled={busy || !form.host.trim() || !form.name.trim()}>Save registry</button>
        </form>
      )}
      <h3 style={{ fontSize: 16, marginTop: 16 }}>Anonymous pulls</h3>
      <StateNotice state={policy.state} onRetry={policy.reload} />
      {policy.state === 'ready' && policy.data && (
        <>
          <label><input type="checkbox" role="switch" checked={policy.data.anonymous_pull_enabled === true} disabled={busy} onChange={(e) => void setAnonymous(e.target.checked)} />Allow anonymous pulls</label>
          <p style={{ color: 'var(--ink)', fontSize: 13 }}>Off: images from hosts without a registry entry cannot be pulled. On: anonymous pulls are allowed; audited.</p>
        </>
      )}
      {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
    </section>
  );
};
