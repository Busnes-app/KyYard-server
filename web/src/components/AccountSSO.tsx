import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';

interface LinkProvider { id: string; name: string }
interface LinkedIdentity { provider: string; name: string; subject: string; linked_at?: string }

export function AccountSSO() {
  const providers = useTenantResource<unknown>('/api/auth/sso');
  const choices = Array.isArray(providers.data) ? providers.data.filter((p: unknown): p is LinkProvider => typeof p === 'object' && p !== null && 'id' in p && typeof p.id === 'string' && 'name' in p && typeof p.name === 'string') : [];
  const linked = useTenantResource<LinkedIdentity[]>('/api/account/sso');
  const [busy, setBusy] = useState(false);
  const [unlinking, setUnlinking] = useState('');
  const [message, setMessage] = useState(new URLSearchParams(window.location.search).get('sso') === 'linked' ? 'Single sign-on linked to your existing account.' : '');
  const link = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    setBusy(true); setMessage('');
    form.reset();
    try {
      const provider = data.get('provider');
      const password = data.get('password');
      if (typeof provider !== 'string' || typeof password !== 'string') return;
      const response = await secureFetch(`/api/sso/${encodeURIComponent(provider)}/link`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) });
      if (!response.ok) { setMessage(response.status === 401 ? 'Check your current KyYard password and sign-in session.' : response.status === 429 ? 'Too many attempts. Wait a minute and try again.' : 'Could not start linking. Sign in locally with an active account and try again.'); return; }
      const body: unknown = await response.json();
      if (typeof body !== 'object' || body === null || !('authorization_url' in body) || typeof body.authorization_url !== 'string') { setMessage('Could not start provider sign-in.'); return; }
      const url = new URL(body.authorization_url);
      if (url.protocol !== 'https:') { setMessage('Provider sign-in requires HTTPS.'); return; }
      window.location.assign(url.href);
    } catch { setMessage('Could not reach the server.'); }
    finally { setBusy(false); }
  };
  const unlink = async (event: React.FormEvent<HTMLFormElement>, identity: LinkedIdentity) => {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    setBusy(true); setMessage('');
    form.reset();
    try {
      const response = await secureFetch(`/api/account/sso/${encodeURIComponent(identity.provider)}/unlink`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: data.get('password'), code: data.get('code') || '' }) });
      if (!response.ok) { setMessage(response.status === 401 ? 'Check your current KyYard password and authenticator code.' : response.status === 429 ? 'Too many attempts. Wait a minute and try again.' : 'Could not unlink this sign-in. Reload and try again.'); return; }
      setUnlinking('');
      setMessage(`${identity.name} sign-in unlinked. Sessions it started have ended.`);
      linked.reload();
    } catch { setMessage('Could not reach the server.'); }
    finally { setBusy(false); }
  };
  const identities = Array.isArray(linked.data) ? linked.data : [];
  // An account the provider owns has nothing to link here: the server refuses both reads.
  if (providers.state === 'denied') return null;
  return <section className="panel"><h2>Connect single sign-on to your account</h2>
    {identities.length > 0 && <>
      <h3>Linked sign-in</h3>
      <p>These identities sign in to this account. Unlink any you do not recognise.</p>
      <ul>{identities.map(i => <li key={i.provider}>
        <strong>{i.name}</strong> as {i.subject}{i.linked_at ? `, linked ${new Date(i.linked_at).toLocaleString()}` : ''}{' '}
        {unlinking === i.provider
          ? <form onSubmit={e => void unlink(e, i)} style={{ display: 'grid', gap: 12, maxWidth: 640 }}>
            <label>Current KyYard password<input name="password" type="password" autoComplete="current-password" required disabled={busy} /></label>
            <label>Authenticator code (if two-factor is on)<input name="code" inputMode="text" autoComplete="one-time-code" maxLength={64} disabled={busy} /></label>
            <span><button className="btn-secondary" disabled={busy}>Unlink {i.name}</button> <button type="button" className="btn-secondary" disabled={busy} onClick={() => setUnlinking('')}>Cancel</button></span>
          </form>
          : <button type="button" className="btn-secondary" disabled={busy} aria-label={`Unlink ${i.name}`} onClick={() => setUnlinking(i.provider)}>Unlink</button>}
      </li>)}</ul>
    </>}
    <p>Enter your current KyYard password, then sign in to the provider as the identity you want to connect. Your account, permissions and local sign-in stay the same.</p>
    {linked.state !== 'loading' && linked.state !== 'denied' && <StateNotice state={linked.state} onRetry={linked.reload} />}
    <StateNotice state={providers.state} onRetry={providers.reload} />
    {providers.state === 'ready' && (choices.length ? <form onSubmit={link} style={{ display: 'grid', gap: 12, maxWidth: 640 }}>
      <label>Sign-in provider<select name="provider" required disabled={busy}>{choices.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>
      <label>Current KyYard password<input name="password" type="password" autoComplete="current-password" required disabled={busy} /></label>
      <button className="btn-secondary" disabled={busy}>Connect single sign-on</button>
    </form> : <p>No enabled sign-in providers are available.</p>)}
    {message && <p role="status">{message}</p>}
  </section>;
}
