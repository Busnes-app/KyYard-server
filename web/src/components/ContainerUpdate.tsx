import { useCallback, useEffect, useRef, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { secureFetch } from '../api';
import type { Container } from '../tenant';
import { Link } from './Link';

const VERDICTS = {
  up_to_date: 'Up to date', update_available: 'Update available', pinned: 'Digest pinned', unknown: 'Image version unknown',
  registry_error: 'Registry check failed', check_failed: 'Update check failed', forbidden: 'Update check not permitted',
  rate_limited: 'Update check limit reached', registry_policy: 'Registry access not configured',
  registry_unauthorized: 'Registry authentication required', registry_not_found: 'Image not found in registry',
  registry_rate_limited: 'Registry rate limit reached', registry_private: 'Registry address blocked', registry_unavailable: 'Registry unavailable',
};
type Verdict = keyof typeof VERDICTS;
type CheckUpdate = (container: Container, signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>;
const CHECK_INTERVAL = 5100; // The server admits 12 checks per actor per minute.
const CACHE_LIFETIME = 5 * 60_000;

function pause(ms: number, signal: AbortSignal): Promise<void> {
  if (ms <= 0 || signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => { window.clearTimeout(timer); signal.removeEventListener('abort', done); resolve(); };
    const timer = window.setTimeout(done, ms);
    signal.addEventListener('abort', done, { once: true });
    if (signal.aborted) done();
  });
}

// One queue for the endpoint page, shared by its visible rows. Cached answers survive
// pagination/filter/tab changes; identity changes and leaving the page invalidate them.
export function useContainerUpdateChecker(base: string): CheckUpdate {
  const queue = useRef(Promise.resolve());
  const next = useRef(0);
  const cache = useRef(new Map<string, { verdict: Verdict; expires: number }>());
  return useCallback((container, signal, fresh) => {
    const key = JSON.stringify([base, container.id, container.image_id, container.image, container.created_at]);
    const task = queue.current.then(async (): Promise<Verdict | null> => {
      if (signal.aborted) return null;
      const saved = cache.current.get(key);
      if (!fresh && saved && saved.expires > Date.now()) return saved.verdict;
      await pause(Math.max(0, next.current - Date.now()), signal);
      if (signal.aborted) return null;
      next.current = Date.now() + CHECK_INTERVAL;
      let verdict: Verdict = 'check_failed';
      try {
        const response = await secureFetch(`${base}/containers/${encodeURIComponent(container.id)}/updates/check`, { method: 'POST', signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]) });
        if (response.status === 401 || response.status === 403) verdict = 'forbidden';
        else if (response.status === 429) verdict = 'rate_limited';
        else {
          const body: unknown = await response.json();
          if (body && typeof body === 'object') {
            if (response.ok && 'image_id' in body && body.image_id === container.image_id && 'verdict' in body && typeof body.verdict === 'string' && body.verdict in VERDICTS) {
              switch (body.verdict) {
                case 'up_to_date': case 'update_available': case 'pinned': case 'unknown': verdict = body.verdict; break;
                case 'registry_error':
                  verdict = 'registry_error';
                  if ('detail' in body) {
                    switch (body.detail) {
                      case 'unauthorized': verdict = 'registry_unauthorized'; break;
                      case 'not_found': verdict = 'registry_not_found'; break;
                      case 'rate_limited': verdict = 'registry_rate_limited'; break;
                      case 'private_destination': verdict = 'registry_private'; break;
                      case 'unavailable': verdict = 'registry_unavailable'; break;
                    }
                  }
                  break;
              }
            } else if ('code' in body && (body.code === 'registry_not_configured' || body.code === 'anonymous_pull_disabled')) verdict = 'registry_policy';
          }
        }
      } catch { /* Fixed failure text only; an aborted request publishes nothing. */ }
      if (signal.aborted) return null;
      if (cache.current.size >= 1000) cache.current.clear();
      cache.current.set(key, { verdict, expires: Date.now() + CACHE_LIFETIME });
      return verdict;
    });
    queue.current = task.then(() => {});
    return task;
  }, [base]);
}

export function ContainerUpdate({ container, active, org, checkUpdate, onUpdate, updateDisabled = false }: { container: Container; active: boolean; org: string; endpoint: string; checkUpdate: CheckUpdate; onUpdate: () => void; updateDisabled?: boolean }) {
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const check = useCallback(async (fresh: boolean) => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    const result = await checkUpdate(container, controller.signal, fresh);
    if (!controller.signal.aborted) { setVerdict(result); setBusy(false); }
  }, [checkUpdate, container.id, container.image_id, container.image, container.created_at]);
  useEffect(() => {
    setVerdict(null);
    if (active) void check(false);
    return () => request.current?.abort();
  }, [active, check]);
  const text = busy ? 'Checking…' : verdict ? VERDICTS[verdict] : 'Endpoint offline';
  return <span className="ky-image-update"><span role="status" className={`badge ${verdict === 'update_available' ? 'badge-warning' : 'badge-secondary'}`}>{text}</span>{verdict === 'registry_policy' && <Link to={`/organizations/${encodeURIComponent(org)}/members#registries-heading`} title="Configure this registry in Members → Registries.">Configure registry access</Link>}<button type="button" className="btn-secondary ky-icon-button" aria-label={`Check image update for ${container.name}`} title="Check image update" disabled={!active || busy} onClick={() => void check(true)}><RefreshCw size={15} /></button>{active && <button type="button" className="btn-secondary" disabled={updateDisabled} onClick={onUpdate} title="Pull the latest image and recreate using current settings">Update image</button>}</span>;
}
