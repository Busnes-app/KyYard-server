import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';

interface LinkProvider { id: string; name: string }

export function AccountSSO() {
  const providers = useTenantResource<unknown>('/api/auth/sso');
  const choices = Array.isArray(providers.data) ? providers.data.filter((p: unknown): p is LinkProvider => typeof p === 'object' && p !== null && 'id' in p && typeof p.id === 'string' && 'name' in p && typeof p.name === 'string') : [];
  const [busy, setBusy] = useState(false);
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
  return <section className="panel"><h2>Connect single sign-on to your account</h2>
    <p>Enter your current KyYard password, then sign in to the provider as the identity you want to connect. Your account, permissions and local sign-in stay the same.</p>
    <StateNotice state={providers.state} onRetry={providers.reload} />
    {providers.state === 'ready' && (choices.length ? <form onSubmit={link} style={{ display: 'grid', gap: 12, maxWidth: 640 }}>
      <label>Sign-in provider<select name="provider" required disabled={busy}>{choices.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>
      <label>Current KyYard password<input name="password" type="password" autoComplete="current-password" required disabled={busy} /></label>
      <button disabled={busy}>Connect single sign-on</button>
    </form> : <p>No enabled sign-in providers are available.</p>)}
    {message && <p role="status">{message}</p>}
  </section>;
}
