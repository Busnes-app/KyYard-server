import { useState } from 'react';
import { tenantWrite, useTenantResource, type ServicePairing, type ServiceToken } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';

export function ServiceTokens({ org }: { org: string }) {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const tokens = useTenantResource<ServiceToken[]>(`${base}/service-tokens`);
  const [pairing, setPairing] = useState<ServicePairing | null>(null);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const pair = async () => {
    setBusy(true);
    setMessage('');
    try {
      const resp = await secureFetch(`${base}/service-tokens/pairings`, { method: 'POST' });
      if (!resp.ok) { setMessage('Could not create a pairing code.'); return; }
      setPairing(await resp.json());
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  const revoke = async (t: ServiceToken) => {
    if (!window.confirm(`Revoke "${t.name}"? kyPulse stops reading this organization immediately. Unpair in kyPulse as well; revoking here does not.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/service-tokens/${encodeURIComponent(t.id)}`, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) tokens.reload();
  };

  return (
    <section className="panel" aria-labelledby="service-tokens-heading">
      <div className="panel-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
        <h2 id="service-tokens-heading" style={{ fontSize: 16 }}>Service tokens</h2>
        <button disabled={busy} onClick={() => void pair()}>Pair kyPulse</button>
      </div>
      <p>A service token lets kyPulse read this organization: endpoints, inventory, samples, container logs and the audit feed. It can change nothing. Unpair in kyPulse as well when you revoke one here; each side keeps its own step.</p>
      {pairing && (
        <div className="dr-alert dr-alert-warn" role="region" aria-label="Pairing code">
          <p>Enter this code in kyPulse before {new Date(pairing.expires_at).toLocaleTimeString()}:</p>
          <pre className="font-mono" style={{ fontSize: 24, letterSpacing: 4 }}>{pairing.code}</pre>
          <p>{pairing.disclosure}</p>
          <button className="btn-secondary" onClick={() => setPairing(null)}>Dismiss</button>
        </div>
      )}
      {message && <p role="alert">{message}</p>}
      <StateNotice state={tokens.state} onRetry={tokens.reload} />
      {tokens.state === 'ready' && tokens.data && (tokens.data.length === 0 ? <p>No service tokens. Pair kyPulse to create one.</p> : (
        <table>
          <thead><tr><th>Service</th><th>Created</th><th>Last used</th><th>From</th><th>Status</th><th /></tr></thead>
          <tbody>
            {tokens.data.map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td>{new Date(t.created_at).toLocaleString()}</td>
                <td>{t.last_used_at ? new Date(t.last_used_at).toLocaleString() : 'never'}</td>
                <td className="font-mono">{t.last_ip ?? ''}</td>
                <td>{t.revoked_at ? 'revoked' : 'active'}</td>
                <td>{!t.revoked_at && <button className="btn-secondary" disabled={busy} onClick={() => void revoke(t)}>Revoke</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ))}
    </section>
  );
}
