import { useCallback, useEffect, useState } from 'react';
import { secureFetch } from './api';

export interface MemberOrganization { id: string; name: string; role: string }
export interface Environment { id: string; organization_id: string; name: string }
export interface Member { user_id: string; username: string; role: string; status: string }
export interface EndpointAlert { id: number; kind: string; details: string; created_at: string }
export interface Endpoint { id: string; environment_id: string; name: string; runtime: string; state: string; facts: Record<string, string>; fingerprint: string; pending_fingerprint?: string; capabilities: string[]; alerts: EndpointAlert[]; created_at: string; approved_by?: string }
export interface EnrollmentToken { id: string; runtime: string; expires_at: string; token: string; command?: string; image?: string; note?: string; disclosure: string }
export interface Port { host_ip?: string; host?: number; container: number; protocol: string }
export interface Container { id: string; name: string; image: string; image_id: string; state: string; status: string; created_at: string; ports: Port[]; labels: Record<string, string>; networks: string[]; compose_project?: string }
export interface Snapshot {
  generation: number; observed_at: string;
  engine: { runtime: string; version: string; api_version: string; os: string; arch: string; kernel: string; cpus: number; memory_bytes: number; hostname: string };
  containers: Container[]; images: { id: string; tags: string[]; digests: string[]; size_bytes: number; created_at: string }[];
  networks: { id: string; name: string; driver: string; scope: string }[]; volumes: { name: string; driver: string; mountpoint: string; created_at: string }[];
  truncated?: string[];
}
export interface Sample { container_id: string; observed_at: string; cpu_percent: number; memory_bytes: number; memory_limit: number; rx_bytes: number; tx_bytes: number; pids: number; restart_count?: number }
export interface Inventory { endpoint_id: string; state: string; generation: number; observed_at: string; received_at: string; snapshot: Snapshot }
export interface AuditRecord { id: number; user_id: string; action: string; resource: string; environment_id: string; correlation_id: string; result: string; created_at: string }

export interface Registry { id: string; host: string; name: string; username: string; has_credential: boolean; allow_private: boolean }
// private_registries_enabled is the operator's KY_REGISTRY_ALLOW_PRIVATE, read-only here.
export interface RegistryPolicy { anonymous_pull_enabled: boolean; private_registries_enabled: boolean }

// Cached update verdicts for one adopted instance; services is [] before the first check.
export interface ImageCheck { service: string; reference: string; local_digest: string; remote_digest: string; verdict: string; detail: string; checked_at: string }
export interface UpdateCheck { instance_id: string; mapping_version: number; services: ImageCheck[] }
export interface ValidationRollback { deployment_id: string; revision: number; outcome: string; detail: string }
// A deployment's health validation (docs/application-schema.md, Health validation).
export interface Validation { deployment_id: string; policy_run_id: string; automated: boolean; is_rollback: boolean; phase: string; started_at: string; observe_until: string; verdict: string; detail: string; rollback: ValidationRollback | null; correlation_id: string; finished_at: string | null }
export interface PolicyRun { id: string; policy_id: string; occurrence: string; started_at: string; finished_at: string | null; outcome: string; deployment_id: string; detail: string; correlation_id: string; validation?: Validation }
export interface UpdatePolicy { id: string; application_id: string; created_by: string; mode: string; timezone: string; weekdays: number[]; start_minute: number; end_minute: number; status: string; paused_reason: string; consecutive_failures: number; created_at: string; updated_at: string; next_occurrence: string | null; runs?: PolicyRun[] }

export const privateDisabled = 'Private-address registries are disabled by the operator (KY_REGISTRY_ALLOW_PRIVATE).';

export const tenantRoles = ['organization_admin', 'environment_admin', 'operator', 'developer', 'read_only'] as const;
// Mirrors permissions.Allows(role, ContainerExec): only organization admins may exec.
export const canExec = (role: string | undefined) => role === 'organization_admin';

// Mirrors permissions.Allows(role, ApplicationPolicy): only organization admins edit update policies.
export const canManagePolicies = (role: string | undefined) => role === 'organization_admin';

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
      const payload = await resp.json() as T;
      if (!live) return;
      setData(payload);
      setState('ready');
    }).catch(() => { if (live) setState('offline'); });
    return () => { live = false; };
  }, [url, refreshKey, tick]);
  return { state, data, reload: useCallback(() => setTick((n) => n + 1), []) };
}

// Platform administration (`/api/admin/*`), platform admin role only.
export interface AdminOrganization { id: string; name: string; created_at: string; members: number }
export interface AdminUser { id: string; username: string; display_name: string; role: string; status: string; sso_provider: string; created_at: string }
export interface CreatedUser { id: string; username: string; display_name: string; role: string; temporary_password: string }

// `texts` lets a screen name its own 403, 400 and 404 refusals and its own 409 codes.
export type WriteTexts = { forbidden?: string; invalid?: string; notFound?: string; conflict?: Record<string, string> };

// Maps a refused response to a fixed message; server text is never shown.
export async function refusal(resp: Response, texts: WriteTexts = {}): Promise<string> {
  const payload = await resp.json().catch(() => ({}));
  if (payload.code === 'last_administrator') return 'At least one active administrator is required.';
  if (payload.code === 'private_registries_disabled') return privateDisabled;
  if (resp.status === 409 && texts.conflict && typeof payload.code === 'string' && Object.hasOwn(texts.conflict, payload.code)) return texts.conflict[payload.code];
  if (resp.status === 403) return texts.forbidden ?? 'You do not have permission to do that.';
  if (resp.status === 400 && texts.invalid) return texts.invalid;
  if (resp.status === 404) return texts.notFound ?? 'Not found in this access scope.';
  if (payload.code === 'environment_in_use') return 'Discard draft applications and revoke every endpoint in this environment before deleting it.';
  if (resp.status === 409) return 'That already exists.';
  return `Request failed (${resp.status}).`;
}

export const offlineWrite = 'Offline: the server could not be reached.';

// Writes return a message for the form instead of throwing; '' means success.
export async function tenantWrite(url: string, method: string, body?: unknown, texts: WriteTexts = {}): Promise<string> {
  try {
    const resp = await secureFetch(url, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    return resp.ok ? '' : await refusal(resp, texts);
  } catch {
    return offlineWrite;
  }
}
