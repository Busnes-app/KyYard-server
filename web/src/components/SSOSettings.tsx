import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';

interface Provider { id: string; name: string; kind: string; enabled: boolean; secret_set: boolean; callback_url: string }
export function SSOSettings() {
  const providers = useTenantResource<Provider[]>('/api/settings/sso');
  const [kind, setKind] = useState('oidc');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const save = async (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault(); const form = e.currentTarget; const data = new FormData(form);
    setBusy(true); setMessage('');
    try {
      const body = Object.fromEntries(data);
      const response = await secureFetch('/api/settings/sso', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...body, kind, enabled: true, auto_provision: data.has('auto_provision') }) });
      if (!response.ok) { setMessage(`Could not save provider (${response.status}). Check the HTTPS URLs and required fields.`); return; }
      form.reset(); setMessage('Provider saved. Register its callback URL below with your identity provider.'); providers.reload();
    } catch { setMessage('Could not reach the server.'); } finally { setBusy(false); }
  };
  const remove = async (p: Provider) => {
    if (!window.confirm(`Remove ${p.name} sign-in? Existing accounts and sessions remain; local login stays available.`)) return;
    setBusy(true);
    try { const r = await secureFetch(`/api/settings/sso/${encodeURIComponent(p.id)}`, { method: 'DELETE' }); setMessage(r.ok ? 'Provider removed.' : `Could not remove provider (${r.status}).`); if (r.ok) providers.reload(); }
    catch { setMessage('Could not reach the server.'); } finally { setBusy(false); }
  };
  const update = async (p: Provider, body: { enabled: boolean } | { client_secret: string }) => {
    setBusy(true); setMessage('');
    try { const r = await secureFetch(`/api/settings/sso/${encodeURIComponent(p.id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); setMessage(r.ok ? 'Provider updated.' : `Update refused (${r.status}).`); if (r.ok) providers.reload(); }
    catch { setMessage('Could not reach the server.'); } finally { setBusy(false); }
  };
  return <section className="panel"><h2>Single sign-on</h2>
    <p>Use KyIdentity or another OpenID Connect provider. For OAuth 2 providers without discovery, supply the authorization, token and profile endpoints. Authorization Code with PKCE (S256) is required.</p>
    <StateNotice state={providers.state} onRetry={providers.reload} />
    {providers.state === 'ready' && <>
      {providers.data?.length === 0 && <p>No providers configured. Local sign-in remains available.</p>}
      {providers.data?.map((p) => <div key={p.id} style={{ margin: '16px 0' }}><strong>{p.name}</strong> · {p.kind} · {p.enabled ? 'enabled' : 'disabled'}<p>Callback: <code>{p.callback_url}</code></p>{p.id.startsWith('idp_') ? <><button className="btn-secondary" disabled={busy} onClick={() => void update(p, { enabled: !p.enabled })}>{p.enabled ? 'Disable' : 'Enable'}</button>{' '}<button className="btn-secondary" disabled={busy} onClick={() => void remove(p)}>Remove provider</button><details><summary>Rotate client secret</summary><form onSubmit={(e) => { e.preventDefault(); const form = e.currentTarget; const secret = new FormData(form).get('secret'); if (typeof secret === 'string') { void update(p, { client_secret: secret }); form.reset(); } }}><label>New client secret<input name="secret" type="password" autoComplete="new-password" required /></label><button disabled={busy}>Save new secret</button></form></details></> : <p>Managed by server environment configuration.</p>}</div>)}
      <details><summary>Add sign-in provider</summary><form onSubmit={save} style={{ display: 'grid', gap: 12, marginTop: 16, maxWidth: 640 }}>
        <label>Provider name<input name="name" required maxLength={80} placeholder="KyIdentity" /></label>
        <label>Protocol<select value={kind} onChange={(e) => setKind(e.target.value)}><option value="oidc">OpenID Connect (including KyIdentity)</option><option value="oauth2">OAuth 2 with a JSON profile endpoint</option></select></label>
        {kind === 'oidc' ? <label>Issuer URL<input name="issuer" type="url" required placeholder="https://identity.example.com" /></label> : <>
          <label>Authorization URL<input name="authorization_url" type="url" required /></label>
          <label>Token URL<input name="token_url" type="url" required /></label>
          <label>Profile / userinfo URL<input name="userinfo_url" type="url" required /></label>
          <label>Scopes (space separated)<input name="scopes" placeholder="read:user user:email" /></label>
          <label>Stable account ID field<input name="subject_field" required defaultValue="id" /></label>
          <label>Username field<input name="username_field" defaultValue="login" /></label>
          <label>Display name field<input name="name_field" defaultValue="name" /></label>
          <label>Email field<input name="email_field" defaultValue="email" /></label>
          <label>Token client authentication<select name="auth_method"><option value="basic">HTTP Basic</option><option value="post">Request body</option></select></label>
          <p>Profile fields may use dot paths, such as data.id. Choose an immutable account ID, never email.</p>
        </>}
        <label>Client ID<input name="client_id" required autoComplete="off" /></label>
        <label>Client secret<input name="client_secret" type="password" autoComplete="new-password" /></label>
        <label><input type="checkbox" name="auto_provision" defaultChecked style={{ width: 'auto' }} /> Create an account on first sign-in. Organization access must be granted separately.</label>
        <button disabled={busy} type="submit">Add provider</button>
      </form></details>
    </>}
    {message && <p role="status">{message}</p>}
  </section>;
}
