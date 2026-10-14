import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';

const FAILURES: Record<number, string> = {
  401: 'Your session has expired. Sign in again.',
  403: 'You do not have permission to refresh this inventory.',
  409: 'The endpoint is offline. Inventory was not refreshed.',
  429: 'Too many refresh requests. Try again in a minute.',
  501: 'Upgrade the host agent to refresh inventory on demand.',
};
const DONE = 'Inventory refreshed.';
// "Refreshed" is only true for a moment; the page's own staleness notice takes over after.
const REFRESHED_FOR_MS = 10_000;

export function InventoryRefresh({ base, onRefresh }: { base: string; onRefresh: () => void }) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  useEffect(() => {
    if (message !== DONE) return;
    const timer = window.setTimeout(() => setMessage(''), REFRESHED_FOR_MS);
    return () => window.clearTimeout(timer);
  }, [message]);
  const refresh = async () => {
    const abort = new AbortController();
    request.current = abort; setBusy(true); setMessage('Waiting for a fresh inventory report…');
    try {
      const response = await secureFetch(`${base}/inventory/refresh`, { method: 'POST', signal: abort.signal });
      if (abort.signal.aborted) return;
      if (response.ok) { setMessage(DONE); onRefresh(); }
      else setMessage(FAILURES[response.status] ?? 'No fresh inventory report arrived. Try again.');
    } catch { if (!abort.signal.aborted) setMessage('The server could not be reached. Inventory was not refreshed.'); }
    finally { if (!abort.signal.aborted) setBusy(false); }
  };
  // The live region stays mounted so screen readers announce the first message too.
  return <div><button className="btn-secondary" disabled={busy} onClick={() => void refresh()}>{busy ? 'Refreshing inventory…' : 'Refresh inventory'}</button><p role="status">{message}</p></div>;
}
