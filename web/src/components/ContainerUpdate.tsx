import { useCallback, useEffect, useRef, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { secureFetch } from '../api';
import type { Container } from '../tenant';
import { Link } from './Link';

export const VERDICTS = {
  up_to_date: 'Up to date', update_available: 'Update available', pinned: 'Digest pinned', unknown: 'Image version unknown',
  registry_error: 'Registry check failed', check_failed: 'Update check failed', forbidden: 'Update check not permitted',
  rate_limited: 'Update check limit reached', registry_policy: 'Registry access not configured',
  registry_unauthorized: 'Registry authentication required', registry_not_found: 'Image not found in registry',
  registry_rate_limited: 'Registry rate limit reached', registry_private: 'Registry address blocked', registry_unavailable: 'Registry unavailable',
  managed: 'Managed by an application',
};
export type Verdict = keyof typeof VERDICTS;
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

const DETAILS: Record<string, Verdict> = { unauthorized: 'registry_unauthorized', not_found: 'registry_not_found', rate_limited: 'registry_rate_limited', private_destination: 'registry_private', unavailable: 'registry_unavailable' };
// readVerdict maps a check answer to a fixed verdict; matches proves the answer is for this row.
export async function readVerdict(response: Response, matches: (body: object) => boolean): Promise<Verdict> {
  if (response.status === 401 || response.status === 403) return 'forbidden';
  if (response.status === 429) return 'rate_limited';
  const body: unknown = await response.json();
  if (!body || typeof body !== 'object') return 'check_failed';
  if (response.ok && matches(body) && 'verdict' in body) {
    switch (body.verdict) {
      case 'up_to_date': case 'update_available': case 'pinned': case 'unknown': case 'managed': return body.verdict;
      case 'registry_error': return 'detail' in body && typeof body.detail === 'string' && Object.hasOwn(DETAILS, body.detail) ? DETAILS[body.detail] ?? 'registry_error' : 'registry_error';
    }
    return 'check_failed';
  }
  if ('code' in body && (body.code === 'registry_not_configured' || body.code === 'anonymous_pull_disabled')) return 'registry_policy';
  return 'check_failed';
}

// One queue per page, shared by its visible rows. Cached answers survive pagination, filter and
// tab changes; identity changes (part of key) and leaving the page invalidate them.
export function useUpdateQueue() {
  const queue = useRef(Promise.resolve());
  const next = useRef(0);
  const cache = useRef(new Map<string, { verdict: Verdict; expires: number }>());
  return useCallback((key: string, url: string, matches: (body: object) => boolean, signal: AbortSignal, fresh: boolean) => {
    const task = queue.current.then(async (): Promise<Verdict | null> => {
      if (signal.aborted) return null;
      const saved = cache.current.get(key);
      if (!fresh && saved && saved.expires > Date.now()) return saved.verdict;
      await pause(Math.max(0, next.current - Date.now()), signal);
      if (signal.aborted) return null;
      next.current = Date.now() + CHECK_INTERVAL;
      let verdict: Verdict = 'check_failed';
      try {
        verdict = await readVerdict(await secureFetch(url, { method: 'POST', signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]) }), matches);
      } catch { /* Fixed failure text only; an aborted request publishes nothing. */ }
      if (signal.aborted) return null;
      if (cache.current.size >= 1000) cache.current.clear();
      cache.current.set(key, { verdict, expires: Date.now() + CACHE_LIFETIME });
      return verdict;
    });
    queue.current = task.then(() => {});
    return task;
  }, []);
}

export function useContainerUpdateChecker(base: string): CheckUpdate {
  const run = useUpdateQueue();
  return useCallback((container, signal, fresh) => run(
    JSON.stringify([base, container.id, container.image_id, container.image, container.created_at]),
    `${base}/containers/${encodeURIComponent(container.id)}/updates/check`,
    (body) => 'image_id' in body && body.image_id === container.image_id, signal, fresh,
  ), [base, run]);
}

// UpdateBadge checks when identity changes or on demand and offers the update; a managed answer
// offers none, because the application's own flow owns those images.
export function UpdateBadge({ label, identity, active, org, check: checkUpdate, onUpdate, updateDisabled = false, updateTitle = 'Pull the latest image and update using current settings' }: { label: string; identity: string; active: boolean; org: string; check: (signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>; onUpdate: () => void; updateDisabled?: boolean; updateTitle?: string }) {
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const latest = useRef(checkUpdate);
  latest.current = checkUpdate;
  // `identity` re-keys the check: callers remount on endpoint change and the queue cache key carries base.
  const check = useCallback(async (fresh: boolean) => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    const result = await latest.current(controller.signal, fresh);
    if (!controller.signal.aborted) { setVerdict(result); setBusy(false); }
  }, [identity]);
  useEffect(() => {
    setVerdict(null);
    if (active) void check(false);
    return () => request.current?.abort();
  }, [active, check]);
  const text = busy ? 'Checking…' : verdict ? VERDICTS[verdict] : 'Endpoint offline';
  return <span className="ky-image-update"><span role="status" className={`badge ${verdict === 'update_available' ? 'badge-warning' : 'badge-secondary'}`}>{text}</span>{verdict === 'registry_policy' && <Link to={`/organizations/${encodeURIComponent(org)}/members#registries-heading`} title="Configure this registry in Members → Registries.">Configure registry access</Link>}<button type="button" className="btn-secondary ky-icon-button" aria-label={`Check image update for ${label}`} title="Check image update" disabled={!active || busy} onClick={() => void check(true)}><RefreshCw size={15} /></button>{active && verdict !== 'managed' && <button type="button" className="btn-secondary" disabled={updateDisabled} onClick={onUpdate} title={updateTitle}>Update image</button>}</span>;
}

export function ContainerUpdate({ container, active, org, checkUpdate, onUpdate, updateDisabled = false }: { container: Container; active: boolean; org: string; endpoint: string; checkUpdate: CheckUpdate; onUpdate: () => void; updateDisabled?: boolean }) {
  return <UpdateBadge label={container.name} identity={JSON.stringify([container.id, container.image_id, container.image, container.created_at])} active={active} org={org}
    check={(signal, fresh) => checkUpdate(container, signal, fresh)} onUpdate={onUpdate} updateDisabled={updateDisabled} updateTitle="Pull the latest image and recreate using current settings" />;
}
