import type { WorkloadConfiguration, WorkloadContainer, WorkloadEnv, WorkloadRef, WorkloadResources } from '../tenant';
import { unsupportedLabel } from './containerConfiguration';

// Codes a workload read adds to unsupported (protocol.WorkloadUnsupportedCodes).
const workloadNames: Record<string, string> = {
  env_field_ref: 'has environment taken from pod fields',
  resources_extended: 'has resources beyond CPU and memory',
  containers_truncated: 'has more containers than the read could carry',
  env_truncated: 'has more environment than the read could carry',
  argv_truncated: 'has a command longer than the read could carry',
  env_from_truncated: 'imports more Secrets and ConfigMaps than the read could carry',
};
// workloadUnsupportedLabel is the fixed text of an unsupported code, '' for one this build does not know.
export const workloadUnsupportedLabel = (code: string): string => Object.hasOwn(workloadNames, code) ? workloadNames[code] ?? '' : unsupportedLabel(code);

export type WorkloadSpec = Omit<WorkloadConfiguration, 'target' | 'observed_at' | 'managed'>;

type Obj = Record<string, unknown>;
const obj = (v: unknown): v is Obj => v !== null && typeof v === 'object' && !Array.isArray(v);
const str = (v: unknown): v is string => typeof v === 'string';
const strs = (v: unknown, max: number): v is string[] => Array.isArray(v) && v.length <= max && v.every(str);
const DNS_LABEL = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const ENV_FROM = /^(secret|configmap)\/[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/;
const STRATEGIES = ['', 'RollingUpdate', 'Recreate', 'OnDelete'];
const QUANTITY_KEYS = ['cpu_request', 'cpu_limit', 'memory_request', 'memory_limit'] as const;

function env(v: unknown): WorkloadEnv[] | null {
  if (!Array.isArray(v) || v.length > 128) return null;
  const out: WorkloadEnv[] = [];
  for (const e of v) {
    if (!obj(e) || !str(e.name) || e.name === '') return null;
    const refs = [e.secret_ref, e.config_map_ref].filter((r) => r !== undefined && r !== '');
    if (e.value !== undefined && !str(e.value)) return null;
    if (refs.some((r) => !str(r)) || refs.length + (e.value ? 1 : 0) > 1) return null;
    out.push(e.secret_ref ? { name: e.name, secret_ref: e.secret_ref as string } : e.config_map_ref ? { name: e.name, config_map_ref: e.config_map_ref as string } : { name: e.name, value: (e.value as string | undefined) ?? '' });
  }
  return out;
}
function containers(v: unknown): WorkloadContainer[] | null {
  if (!Array.isArray(v) || v.length > 16) return null;
  const out: WorkloadContainer[] = [];
  for (const c of v) {
    if (!obj(c) || !str(c.name) || !DNS_LABEL.test(c.name) || !str(c.image) || !str(c.image_id) || !strs(c.command, 64) || !strs(c.args, 64) || !obj(c.resources)) return null;
    const e = env(c.env);
    const r: Partial<WorkloadResources> = {};
    for (const k of QUANTITY_KEYS) {
      const q = c.resources[k];
      if (!str(q) || q.length > 32) return null;
      r[k] = q;
    }
    if (!e) return null;
    out.push({ name: c.name, image: c.image, image_id: c.image_id, command: [...c.command], args: [...c.args], env: e, resources: r as WorkloadResources });
  }
  return out;
}

// parseWorkloadConfiguration is the boundary check on a workload read: a payload for another
// workload, or not the wire shape, is refused whole.
export function parseWorkloadConfiguration(value: unknown, target: WorkloadRef): WorkloadConfiguration | null {
  if (!obj(value) || !obj(value.target) || value.target.namespace !== target.namespace || value.target.kind !== target.kind || value.target.name !== target.name) return null;
  const v = value;
  if (!str(v.observed_at) || !Number.isFinite(Date.parse(v.observed_at)) || !str(v.resource_version) || v.resource_version.length > 64) return null;
  if (v.replicas !== undefined && (typeof v.replicas !== 'number' || !Number.isInteger(v.replicas) || v.replicas < 0 || v.replicas > 1000)) return null;
  if (typeof v.paused !== 'boolean' || typeof v.managed !== 'boolean' || !str(v.strategy) || !STRATEGIES.includes(v.strategy)) return null;
  const c = containers(v.containers), i = containers(v.init_containers ?? []);
  if (!c || !i || c.length === 0) return null;
  const names = [...c, ...i].map((x) => x.name);
  if (new Set(names).size !== names.length) return null;
  const f = v.env_from ?? [];
  if (!strs(f, 64) || new Set(f).size !== f.length || !f.every((e) => ENV_FROM.test(e))) return null;
  const u = v.unsupported ?? [];
  if (!strs(u, 16) || new Set(u).size !== u.length || !u.every((code) => workloadUnsupportedLabel(code) !== '')) return null;
  const out: WorkloadConfiguration = {
    target: { namespace: target.namespace, kind: target.kind, name: target.name }, observed_at: v.observed_at, resource_version: v.resource_version,
    paused: v.paused, strategy: v.strategy, containers: c, init_containers: i, env_from: [...f], managed: v.managed, unsupported: [...u],
  };
  if (v.replicas !== undefined) out.replicas = v.replicas as number;
  return out;
}

// toWorkloadSpec drops the read-time identity. resource_version and unsupported stay: the apply
// echoes them so the server can refuse a stale or incomplete edit.
export function toWorkloadSpec(c: WorkloadConfiguration): WorkloadSpec {
  const { target: _target, observed_at: _observed, managed: _managed, ...spec } = structuredClone(c);
  return spec;
}

// diffWorkload names the changed paths, never values (env holds references and secrets' names).
export function diffWorkload(before: WorkloadSpec, after: WorkloadSpec): string[] {
  const out: string[] = [];
  const differs = (a: unknown, b: unknown) => JSON.stringify(a) !== JSON.stringify(b);
  for (const k of ['replicas', 'paused', 'strategy', 'env_from', 'unsupported', 'resource_version'] as const) if (differs(before[k], after[k])) out.push(k);
  for (const group of ['containers', 'init_containers'] as const) {
    const a = new Map(before[group].map((c) => [c.name, c])), b = new Map(after[group].map((c) => [c.name, c]));
    for (const name of new Set([...a.keys(), ...b.keys()])) {
      const x = a.get(name), y = b.get(name);
      if (!x || !y) { out.push(`${group}.${name}`); continue; }
      for (const f of ['image', 'command', 'args', 'env', 'resources'] as const) if (differs(x[f], y[f])) out.push(`${group}.${name}.${f}`);
    }
  }
  return out;
}
