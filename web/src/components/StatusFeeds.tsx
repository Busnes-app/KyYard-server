import { useState } from 'react';
import { tenantWrite, useTenantResource, type StatusFeed, type StatusFeedIssue } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';

export function StatusFeeds({ org }: { org: string }) {
  const base = `/api/organizations/${encodeURIComponent(org)}/status-feeds`;
  const feeds = useTenantResource<StatusFeed[]>(base);
  const [name, setName] = useState('');
  const [issued, setIssued] = useState<StatusFeedIssue | null>(null);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const create = async () => {
    setBusy(true);
    setMessage('');
    try {
      const resp = await secureFetch(base, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: name.trim() }) });
      if (resp.status === 409) { setMessage('This organization has 20 status feeds. Revoke one before creating another.'); return; }
      if (!resp.ok) { setMessage('Could not create the status feed. Names are 1 to 64 characters.'); return; }
      setIssued(await resp.json());
      setName('');
      feeds.reload();
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  const revoke = async (f: StatusFeed) => {
    if (!window.confirm(`Revoke "${f.name}"? Its URL stops working immediately.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/${encodeURIComponent(f.id)}`, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) feeds.reload();
  };

  return (
    <section className="panel" aria-labelledby="status-feeds-heading">
      <div className="panel-header">
        <h2 id="status-feeds-heading" style={{ fontSize: 16 }}>Status feeds (RSS)</h2>
      </div>
      <p>A status feed is a secret RSS URL for down monitoring that needs no sign-in. It lists offline endpoints, unhealthy, restarting, dead or exited containers (exited only when the restart policy is <code>always</code> or <code>unless-stopped</code>), and cluster nodes that are not ready. An empty feed means nothing is down; if the feed cannot be fetched, KyYard itself is down. Anyone with the URL sees those names, so give it only to your monitoring.</p>
      <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void create(); }}>
        <label>Feed name<input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} disabled={busy} autoComplete="off" placeholder="uptime monitor" /></label>
        <button disabled={busy || name.trim() === ''}>Create status feed</button>
      </form>
      {issued && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="New status feed">
          <p>Feed URL for "{issued.status_feed.name}". {issued.disclosure}</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{issued.url}</pre>
          <button className="btn-secondary" onClick={() => setIssued(null)}>I have saved the URL</button>
        </div>
      )}
      {message && <p role="status">{message}</p>}
      <StateNotice state={feeds.state} onRetry={feeds.reload} />
      {feeds.state === 'ready' && feeds.data && (feeds.data.length === 0 ? <p>No status feeds.</p> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Feed</th><th>Created by</th><th>Created</th><th>Last read</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>
              {feeds.data.map((f) => (
                <tr key={f.id}>
                  <td data-label="Feed">{f.name}</td>
                  <td data-label="Created by">{f.created_by_name || 'a removed account'}</td>
                  <td data-label="Created">{new Date(f.created_at).toLocaleDateString()}</td>
                  <td data-label="Last read">{f.last_used_at ? `${new Date(f.last_used_at).toLocaleString()}${f.last_ip ? ` from ${f.last_ip}` : ''}` : 'never'}</td>
                  <td data-label="Actions"><button className="btn-secondary" disabled={busy} aria-label={`Revoke ${f.name}`} onClick={() => void revoke(f)}>Revoke</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </section>
  );
}
