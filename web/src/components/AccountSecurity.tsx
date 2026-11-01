import { useState } from 'react';
import type { FormEvent } from 'react';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';

interface Me { user?: { sso_provider?: string; totp_enabled?: boolean } }
interface Enrolment { secret: string; otpauth_uri: string }

const formStyle = { display: 'grid', gap: 12, maxWidth: 640 };
// Submit buttons size to their label, like the other panel actions.
const submitStyle = { justifySelf: 'start' } as const;

async function failure(response: Response): Promise<string> {
  if (response.status === 401) return 'Check your current password and code.';
  if (response.status === 403) return 'This account cannot change its sign-in security here.';
  if (response.status === 429) return 'Too many attempts. Wait a few minutes and try again.';
  const body: unknown = await response.json().catch(() => null);
  if ((response.status === 400 || response.status === 409) && typeof body === 'object' && body !== null && 'error' in body && typeof body.error === 'string') return body.error;
  return 'The change failed. Try again.';
}

// post sends the named fields present in the form, clears its inputs and returns the response, or a message.
async function post(path: string, form: HTMLFormElement, fields: string[]): Promise<Response | string> {
  const data = new FormData(form);
  const body = Object.fromEntries(fields.flatMap((name) => { const value = data.get(name); return typeof value === 'string' ? [[name, value]] : []; }));
  form.reset();
  try {
    const response = await secureFetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    return response.ok ? response : await failure(response);
  } catch { return 'Could not reach the server.'; }
}

function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [copied, setCopied] = useState('');
  const text = codes.join('\n');
  return <div role="region" aria-label="New recovery codes">
    <p><strong>Save these recovery codes now. They are shown only once.</strong> Each one signs you in once if you lose your authenticator.</p>
    <textarea readOnly aria-label="Recovery codes" rows={codes.length} value={text} style={{ fontFamily: 'monospace', width: '100%', maxWidth: 320 }} />
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12 }}>
      <button type="button" className="btn-secondary" onClick={async () => {
        try { await navigator.clipboard.writeText(text); setCopied('Copied.'); } catch { setCopied('Copy failed; select the codes and copy them by hand.'); }
      }}>Copy recovery codes</button>
      <button type="button" onClick={onDone}>I have saved these codes</button>
    </div>
    {copied && <p role="status">{copied}</p>}
  </div>;
}

export function AccountSecurity() {
  const me = useTenantResource<Me>('/api/auth/me');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [enrolment, setEnrolment] = useState<Enrolment | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const user = me.data?.user;
  const enabled = user?.totp_enabled === true;

  const submit = (path: string, fields: string[], done: (response: Response) => Promise<void>) => async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const next = form.elements.namedItem('new_password');
    const confirmation = form.elements.namedItem('confirm_password');
    if (next instanceof HTMLInputElement && confirmation instanceof HTMLInputElement && next.value !== confirmation.value) { setMessage('The new passwords do not match.'); return; }
    const pending = post(path, form, fields);
    setBusy(true); setMessage('');
    const result = await pending;
    if (typeof result === 'string') setMessage(result);
    else await done(result).catch(() => setMessage('Unexpected server response.'));
    setBusy(false);
  };
  const showCodes = async (response: Response) => {
    const body: unknown = await response.json();
    if (typeof body !== 'object' || body === null || !('recovery_codes' in body) || !Array.isArray(body.recovery_codes)) throw new Error();
    setEnrolment(null);
    setCodes(body.recovery_codes.filter((c: unknown): c is string => typeof c === 'string'));
  };

  if (me.state !== 'ready') return <section className="panel"><h2>Account security</h2><StateNotice state={me.state} onRetry={me.reload} /></section>;
  if (user?.sso_provider !== 'local') return <section className="panel"><h2>Account security</h2><p>This account signs in through its identity provider. Manage its password and two-factor authentication there.</p></section>;

  const codeInput = (label: string) => <label>{label}<input name="code" inputMode="text" autoComplete="one-time-code" required maxLength={64} disabled={busy} /></label>;
  const passwordInput = <label>Current password<input name="password" type="password" autoComplete="current-password" required maxLength={1024} disabled={busy} /></label>;

  return <>
    {message && <p role="status">{message}</p>}
    <section className="panel">
      <h2>Two-factor authentication</h2>
      {codes ? <RecoveryCodes codes={codes} onDone={() => { setCodes(null); me.reload(); }} />
        : !enabled && !enrolment ? <>
          <p>Two-factor authentication is off. Turn it on to require a code from an authenticator app at sign-in.</p>
          <button disabled={busy} onClick={async () => {
            setBusy(true); setMessage('');
            try {
              const response = await secureFetch('/api/account/mfa/enrol', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
              if (!response.ok) { setMessage(await failure(response)); return; }
              const body: unknown = await response.json();
              if (typeof body !== 'object' || body === null || !('secret' in body) || typeof body.secret !== 'string' || !('otpauth_uri' in body) || typeof body.otpauth_uri !== 'string') { setMessage('Unexpected server response.'); return; }
              setEnrolment({ secret: body.secret, otpauth_uri: body.otpauth_uri });
            } catch { setMessage('Could not reach the server.'); } finally { setBusy(false); }
          }}>Set up two-factor authentication</button>
        </> : enrolment ? <form onSubmit={submit('/api/account/mfa/confirm', ['password', 'code'], showCodes)} style={formStyle} aria-label="Turn on two-factor authentication">
          <p>Add KyYard to your authenticator app with this key, or open the setup link on the device that runs the app. Then enter the code it shows.</p>
          <label>Setup key<input readOnly value={enrolment.secret} style={{ fontFamily: 'monospace' }} /></label>
          <p><a href={enrolment.otpauth_uri}>Open in authenticator app</a></p>
          {passwordInput}
          {codeInput('Authenticator code')}
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12 }}>
            <button disabled={busy}>Turn on two-factor authentication</button>
            <button type="button" className="btn-secondary" disabled={busy} onClick={() => setEnrolment(null)}>Cancel</button>
          </div>
        </form> : <>
          <p>Two-factor authentication is on.</p>
          <form onSubmit={submit('/api/account/mfa/recovery-codes', ['code'], showCodes)} style={formStyle} aria-label="Replace recovery codes">
            <h3>Replace recovery codes</h3>
            <p>Your current recovery codes stop working.</p>
            {codeInput('Authenticator code')}
            <button disabled={busy} style={submitStyle}>Replace recovery codes</button>
          </form>
          <form onSubmit={submit('/api/account/mfa/disable', ['password', 'code'], async () => { setMessage('Two-factor authentication is off.'); me.reload(); })} style={formStyle} aria-label="Turn off two-factor authentication">
            <h3>Turn off two-factor authentication</h3>
            {passwordInput}
            {codeInput('Authenticator or recovery code')}
            <button className="btn-danger" disabled={busy}>Turn off two-factor authentication</button>
          </form>
        </>}
    </section>
    <section className="panel">
      <h2>Change password</h2>
      <p>Use at least 12 characters. Your other sessions are signed out.</p>
      <form onSubmit={submit('/api/account/password', ['password', 'new_password', 'code'], async (response) => {
        const body: unknown = await response.json();
        if (typeof body === 'object' && body !== null && 'login_required' in body && body.login_required === true) { window.location.reload(); return; }
        setMessage('Password changed. Your other sessions were signed out.');
      })} style={formStyle} aria-label="Change password">
        {passwordInput}
        <label>New password<input name="new_password" type="password" autoComplete="new-password" required minLength={12} maxLength={1024} disabled={busy} /></label>
        <label>Confirm new password<input name="confirm_password" type="password" autoComplete="new-password" required minLength={12} maxLength={1024} disabled={busy} /></label>
        {enabled && codeInput('Authenticator code')}
        <button className="btn-secondary" disabled={busy} style={submitStyle}>Change password</button>
      </form>
    </section>
  </>;
}
