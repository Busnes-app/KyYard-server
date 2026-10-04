import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';

export function InventoryRefresh({ base, onRefresh }: { base: string; onRefresh: () => void }) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  const refresh = async () => {
    const abort = new AbortController();
    request.current = abort; setBusy(true); setMessage('Waiting for a fresh inventory report…');
    try {
      const response = await secureFetch(`${base}/inventory/refresh`, { method: 'POST', signal: abort.signal });
      if (abort.signal.aborted) return;
      if (response.ok) { setMessage('Inventory refreshed.'); onRefresh(); }
      else setMessage(response.status === 501 ? 'Upgrade the host agent to refresh inventory on demand.'
        : response.status === 403 ? 'You do not have permission to refresh this inventory.'
        : response.status === 409 ? 'The endpoint is offline. Inventory was not refreshed.'
        : response.status === 429 ? 'Too many refresh requests. Try again in a minute.'
        : 'No fresh inventory report arrived. Try again.');
    } catch { if (!abort.signal.aborted) setMessage('The server could not be reached. Inventory was not refreshed.'); }
    finally { if (!abort.signal.aborted) setBusy(false); }
  };
  return <div><button className="btn-secondary" disabled={busy} onClick={() => void refresh()}>{busy ? 'Refreshing inventory…' : 'Refresh inventory'}</button>{message && <p role="status">{message}</p>}</div>;
}
