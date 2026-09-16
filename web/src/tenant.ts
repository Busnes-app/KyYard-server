import { useCallback, useEffect, useState } from 'react';
import { secureFetch } from './api';

export interface MemberOrganization { id: string; name: string; role: string }
export interface Environment { id: string; organization_id: string; name: string }
export interface Member { user_id: string; username: string; role: string; status: string }
export interface AuditRecord { id: number; user_id: string; action: string; resource: string; environment_id: string; correlation_id: string; result: string; created_at: string }

export const tenantRoles = ['organization_admin', 'environment_admin', 'operator', 'developer', 'read_only'] as const;

// Every tenant screen shows exactly one of these; there is no client-side cache to go stale.
export type LoadState = 'loading' | 'ready' | 'denied' | 'notfound' | 'offline' | 'error';

export function stateFor(status: number): LoadState {
  if (status === 401 || status === 403) return 'denied';
  if (status === 404) return 'notfound';
  return 'error';
}

// refreshKey forces a re-read when context changes without the URL changing.
export function useTenantResource<T>(url: string, refreshKey = ''): { state: LoadState; data: T | null; reload: () => void } {
  const [state, setState] = useState<LoadState>('loading');
  const [data, setData] = useState<T | null>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    setState('loading');
    setData(null);
    fetch(url).then(async (resp) => {
      if (!live) return;
      if (!resp.ok) { setState(stateFor(resp.status)); return; }
      setData((await resp.json()) as T);
      setState('ready');
    }).catch(() => { if (live) setState('offline'); });
    return () => { live = false; };
  }, [url, refreshKey, tick]);
  return { state, data, reload: useCallback(() => setTick((n) => n + 1), []) };
}

// Writes return a message for the form instead of throwing; 409 codes are user-facing.
export async function tenantWrite(url: string, method: string, body?: unknown): Promise<string> {
  try {
    const resp = await secureFetch(url, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    if (resp.ok) return '';
    const payload = await resp.json().catch(() => ({}));
    if (payload.code === 'last_administrator') return 'The organization needs at least one active administrator.';
    if (resp.status === 403) return 'You do not have permission to do that in this organization.';
    if (resp.status === 404) return 'Not found in this organization.';
    if (resp.status === 409) return 'That already exists.';
    return `Request failed (${resp.status}).`;
  } catch {
    return 'Offline: the server could not be reached.';
  }
}
