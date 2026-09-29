import type { ConfigHealthcheck, ConfigMount, ConfigNetwork, ConfigPort, ContainerConfiguration, ExplicitSpec } from '../tenant';
import { unsupportedNames, type InspectionTarget } from './ApplicationInspection';

export type Spec = ExplicitSpec;

// Truncation codes a configuration read adds to the unsupported list (protocol configurationCodes).
const truncationNames: Record<string, string> = {
  env_truncated: 'has more environment than the read could carry',
  labels_truncated: 'has more labels than the read could carry',
  argv_truncated: 'has a command or entrypoint longer than the read could carry',
};
const LIST_TRUNCATED = /^list_truncated:[a-z_]+$/;
const HOST_CONFIG = /^host_config:[A-Za-z0-9]{1,64}$/;
const knownCode = (c: string) => Object.hasOwn(unsupportedNames, c) || Object.hasOwn(truncationNames, c) || LIST_TRUNCATED.test(c) || HOST_CONFIG.test(c);
// unsupportedLabel is the fixed text of an unsupported code, '' for one this build does not know.
export function unsupportedLabel(code: string): string {
  if (Object.hasOwn(unsupportedNames, code)) return unsupportedNames[code] ?? '';
  if (Object.hasOwn(truncationNames, code)) return truncationNames[code] ?? '';
  if (HOST_CONFIG.test(code)) return `host setting ${code.slice('host_config:'.length)} (not editable here)`;
  return LIST_TRUNCATED.test(code) ? `has more ${code.slice('list_truncated:'.length)} entries than the read could carry` : '';
}

type Obj = Record<string, unknown>;
const obj = (v: unknown): v is Obj => v !== null && typeof v === 'object' && !Array.isArray(v);
const int = (v: unknown): v is number => typeof v === 'number' && Number.isInteger(v);
const strs = (v: unknown, max: number): v is string[] => Array.isArray(v) && v.length <= max && v.every(s => typeof s === 'string');
const strMap = (v: unknown, max: number): v is Record<string, string> => obj(v) && Object.keys(v).length <= max && Object.values(v).every(s => typeof s === 'string');
const RESTARTS = ['no', 'always', 'unless-stopped', 'on-failure'];

function ports(v: unknown): ConfigPort[] | null {
  if (!Array.isArray(v) || v.length > 128) return null;
  const out: ConfigPort[] = [];
  for (const p of v) {
    if (!obj(p) || !int(p.container) || p.container < 1 || p.container > 65535 || (p.protocol !== 'tcp' && p.protocol !== 'udp')) return null;
    const host = p.host ?? 0;
    if (!int(host) || host < 0 || host > 65535 || (p.host_ip !== undefined && typeof p.host_ip !== 'string')) return null;
    out.push(p.host_ip ? { host_ip: p.host_ip as string, host, container: p.container, protocol: p.protocol } : { host, container: p.container, protocol: p.protocol });
  }
  return out;
}
function mounts(v: unknown): ConfigMount[] | null {
  if (!Array.isArray(v) || v.length > 32) return null;
  const out: ConfigMount[] = [];
  for (const m of v) {
    if (!obj(m) || (m.kind !== 'volume' && m.kind !== 'bind' && m.kind !== 'tmpfs') || typeof m.source !== 'string' || typeof m.target !== 'string' || (m.read_only !== undefined && typeof m.read_only !== 'boolean')) return null;
    out.push({ kind: m.kind, source: m.source, target: m.target, read_only: m.read_only ?? false });
  }
  return out;
}
function networks(v: unknown): ConfigNetwork[] | null {
  if (!Array.isArray(v) || v.length > 16) return null;
  const out: ConfigNetwork[] = [];
  for (const n of v) {
    const aliases = obj(n) ? n.aliases ?? [] : null;
    if (!obj(n) || typeof n.name !== 'string' || !strs(aliases, 32) || (n.ip !== undefined && typeof n.ip !== 'string')) return null;
    out.push({ name: n.name, aliases: [...aliases], ip: n.ip ?? '' });
  }
  return out;
}
function healthcheck(v: unknown): ConfigHealthcheck | null | undefined {
  if (v === null || v === undefined) return null;
  if (!obj(v) || !strs(v.test, 64) || typeof v.interval_seconds !== 'number' || typeof v.timeout_seconds !== 'number' || typeof v.start_period_seconds !== 'number' || !int(v.retries)) return undefined;
  return { test: [...v.test], interval_seconds: v.interval_seconds, timeout_seconds: v.timeout_seconds, start_period_seconds: v.start_period_seconds, retries: v.retries };
}

// parseConfiguration is the boundary check on a configuration read: a payload that is not
// exactly this container's, or not the wire shape, is refused whole.
export function parseConfiguration(value: unknown, target: InspectionTarget): ContainerConfiguration | null {
  if (!obj(value) || !obj(value.target) || value.target.container_id !== target.container_id || value.target.image_id !== target.image_id || value.target.created_unix !== target.created_unix) return null;
  const v = value;
  if (typeof v.observed_at !== 'string' || !Number.isFinite(Date.parse(v.observed_at)) || typeof v.name !== 'string' || typeof v.image_id !== 'string') return null;
  if (!obj(v.image) || typeof v.image.reference !== 'string' || typeof v.image.digest !== 'string' || (v.image.tag !== undefined && typeof v.image.tag !== 'string')) return null;
  if (!strs(v.command, 64) || !strs(v.entrypoint, 64) || typeof v.user !== 'string' || typeof v.working_dir !== 'string' || typeof v.hostname !== 'string') return null;
  if (!Array.isArray(v.env) || v.env.length > 512 || !v.env.every(e => obj(e) && typeof e.name === 'string' && typeof e.value === 'string')) return null;
  if (!strMap(v.labels, 256) || typeof v.restart !== 'string' || !RESTARTS.includes(v.restart) || !int(v.restart_retries) || v.restart_retries < 0) return null;
  const p = ports(v.ports), m = mounts(v.mounts), n = networks(v.networks), h = healthcheck(v.healthcheck);
  if (!p || !m || !n || h === undefined || typeof v.network_mode !== 'string') return null;
  const r = v.resources;
  if (!obj(r) || !int(r.nano_cpus) || !int(r.memory_bytes) || !int(r.memory_swap_bytes) || !int(r.pids_limit)) return null;
  if (typeof v.privileged !== 'boolean' || typeof v.read_only_rootfs !== 'boolean' || typeof v.init !== 'boolean' || typeof v.tty !== 'boolean' || typeof v.stdin_open !== 'boolean') return null;
  if (!strs(v.cap_add, 32) || !strs(v.cap_drop, 32) || !strs(v.security_opt, 32) || !strs(v.extra_hosts, 32) || !strs(v.dns, 32)) return null;
  if (!Array.isArray(v.devices) || v.devices.length > 32 || !v.devices.every(d => obj(d) && typeof d.host === 'string' && typeof d.container === 'string' && typeof d.permissions === 'string')) return null;
  const log = v.log;
  const options = obj(log) ? log.options ?? {} : null;
  if (!obj(log) || typeof log.driver !== 'string' || !strMap(options, 16)) return null;
  if (typeof v.stop_signal !== 'string' || (v.stop_timeout !== undefined && (!int(v.stop_timeout) || v.stop_timeout < 0))) return null;
  const u = v.unsupported;
  if (!strs(u, 128) || new Set(u).size !== u.length || !u.every(knownCode)) return null;
  const devices = (v.devices as Obj[]).map(d => ({ host: d.host as string, container: d.container as string, permissions: d.permissions as string }));
  const config: ContainerConfiguration = {
    target: { container_id: target.container_id, image_id: target.image_id, created_unix: target.created_unix },
    observed_at: v.observed_at, name: v.name,
    image: { reference: v.image.reference, digest: v.image.digest, ...(v.image.tag ? { tag: v.image.tag } : {}) }, image_id: v.image_id,
    command: [...v.command], entrypoint: [...v.entrypoint], user: v.user, working_dir: v.working_dir, hostname: v.hostname,
    env: (v.env as Obj[]).map(e => ({ name: e.name as string, value: e.value as string })), labels: { ...v.labels },
    restart: v.restart, restart_retries: v.restart_retries, ports: p, mounts: m, network_mode: v.network_mode, networks: n,
    resources: { nano_cpus: r.nano_cpus, memory_bytes: r.memory_bytes, memory_swap_bytes: r.memory_swap_bytes, pids_limit: r.pids_limit },
    healthcheck: h, privileged: v.privileged, read_only_rootfs: v.read_only_rootfs, init: v.init, tty: v.tty, stdin_open: v.stdin_open,
    cap_add: [...v.cap_add], cap_drop: [...v.cap_drop], security_opt: [...v.security_opt], extra_hosts: [...v.extra_hosts], dns: [...v.dns],
    devices, log: { driver: log.driver, options: { ...options } }, stop_signal: v.stop_signal, unsupported: [...u],
  };
  if (v.stop_timeout !== undefined) config.stop_timeout = v.stop_timeout as number;
  return config;
}

// toSpec drops the read-time identity. Unsupported stays: the form echoes it so the server can
// refuse a spec built from an incomplete read.
export function toSpec(c: ContainerConfiguration): Spec {
  const { target: _target, observed_at: _observed, ...spec } = structuredClone(c);
  return spec;
}

// needsAck mirrors the server's bind rule: an old bind covers a new one only with the same source
// and target and no less restriction, so a retargeted or writable-made bind needs acknowledgement.
export function needsAck(old: ConfigMount[], m: ConfigMount): boolean {
  return m.kind === 'bind' && !old.some((o) => o.kind === 'bind' && o.source === m.source && o.target === m.target && (!o.read_only || m.read_only));
}

// diff names the top-level fields that differ, never their values (env holds secrets).
export function diff(before: Spec, after: Spec): string[] {
  const keys = new Set([...Object.keys(before), ...Object.keys(after)]) as Set<keyof Spec>;
  return [...keys].filter(k => JSON.stringify(before[k]) !== JSON.stringify(after[k]));
}
