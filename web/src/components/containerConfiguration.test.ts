import { expect, it } from 'vitest';
import { diff, parseConfiguration, toSpec, unsupportedLabel } from './containerConfiguration';

const target = { container_id: 'a'.repeat(64), image_id: `sha256:${'b'.repeat(64)}`, created_unix: 7 };
const payload = (): Record<string, unknown> => ({
  target, observed_at: '2026-09-29T12:00:00Z', name: 'web', image: { reference: 'nginx:1', digest: '' }, image_id: target.image_id,
  command: ['nginx'], entrypoint: [], user: '', working_dir: '', hostname: '',
  env: [{ name: 'A', value: '1' }], labels: { k: 'v' }, restart: 'no', restart_retries: 0,
  ports: [{ container: 80, protocol: 'tcp' }, { host: 8080, container: 80, protocol: 'tcp', host_ip: '127.0.0.1' }],
  mounts: [{ kind: 'volume', source: 'data', target: '/data' }, { kind: 'tmpfs', source: '', target: '/tmp' }],
  network_mode: 'default', networks: [{ name: 'net', aliases: [], ip: '' }],
  resources: { nano_cpus: 0, memory_bytes: 0, memory_swap_bytes: 0, pids_limit: 0 }, healthcheck: null,
  privileged: false, read_only_rootfs: false, init: false, tty: false, stdin_open: false,
  cap_add: [], cap_drop: [], security_opt: [], extra_hosts: [], dns: [], devices: [], log: { driver: 'json-file' },
  stop_signal: '', unsupported: [],
});

it('accepts a well-formed payload and normalises omitted fields', () => {
  const c = parseConfiguration(payload(), target);
  expect(c?.name).toBe('web');
  expect(c?.ports[0]).toMatchObject({ host: 0, container: 80 });
  expect(c?.mounts[0]?.read_only).toBe(false);
  expect(c?.healthcheck).toBeNull();
  expect(c?.log.options).toEqual({});
});
it('rejects a wrong target', () => {
  expect(parseConfiguration(payload(), { ...target, created_unix: 8 })).toBeNull();
  expect(parseConfiguration({ ...payload(), target: { ...target, container_id: 'c'.repeat(64) } }, target)).toBeNull();
});
it('rejects unknown unsupported codes but accepts the truncation codes', () => {
  expect(parseConfiguration({ ...payload(), unsupported: ['made_up'] }, target)).toBeNull();
  expect(parseConfiguration({ ...payload(), unsupported: ['dns', 'dns'] }, target)).toBeNull();
  const ok = parseConfiguration({ ...payload(), unsupported: ['dns', 'env_truncated', 'labels_truncated', 'argv_truncated', 'list_truncated:cap_add'] }, target);
  expect(ok?.unsupported).toHaveLength(5);
  expect(parseConfiguration({ ...payload(), unsupported: ['list_truncated:Bad!'] }, target)).toBeNull();
  expect(unsupportedLabel('list_truncated:cap_add')).toContain('cap_add');
  expect(unsupportedLabel('env_truncated')).not.toBe('');
  expect(unsupportedLabel('made_up')).toBe('');
});
it('rejects missing arrays and malformed members', () => {
  for (const key of ['command', 'entrypoint', 'env', 'ports', 'mounts', 'networks', 'cap_add', 'cap_drop', 'security_opt', 'extra_hosts', 'dns', 'devices', 'unsupported']) {
    const p = payload(); delete p[key];
    expect(parseConfiguration(p, target), key).toBeNull();
  }
  expect(parseConfiguration({ ...payload(), mounts: [{ kind: 'weird', source: '', target: '/x' }] }, target)).toBeNull();
  expect(parseConfiguration({ ...payload(), restart: 'sometimes' }, target)).toBeNull();
  expect(parseConfiguration({ ...payload(), env: [{ name: 'A' }] }, target)).toBeNull();
  expect(parseConfiguration({ ...payload(), labels: { k: 1 } }, target)).toBeNull();
  expect(parseConfiguration(null, target)).toBeNull();
});
it('toSpec strips target and observed_at only, and round-trips', () => {
  const c = parseConfiguration({ ...payload(), unsupported: ['dns'] }, target)!;
  const s = toSpec(c);
  expect(s).not.toHaveProperty('target');
  expect(s).not.toHaveProperty('observed_at');
  expect(s.unsupported).toEqual(['dns']);
  expect(s.name).toBe('web');
  expect(diff(s, toSpec(c))).toEqual([]);
  expect(toSpec(c).env).not.toBe(c.env);
});
it('diff names changed fields and never returns values', () => {
  const a = toSpec(parseConfiguration(payload(), target)!);
  const b = toSpec(parseConfiguration(payload(), target)!);
  expect(diff(a, b)).toEqual([]);
  b.env = [{ name: 'A', value: 'secret' }];
  expect(diff(a, b)).toEqual(['env']);
  b.env = a.env; b.restart = 'always'; b.ports = [];
  expect(diff(a, b)).toEqual(['restart', 'ports']);
  b.restart = 'no'; b.ports = a.ports; b.resources = { ...a.resources, pids_limit: 5 };
  expect(diff(a, b)).toEqual(['resources']);
});

it('accepts host_config codes and labels them with fixed text', () => {
  const c = parseConfiguration({ ...payload(), unsupported: ['host_config:ShmSize', 'dns'] }, target);
  expect(c?.unsupported).toEqual(['host_config:ShmSize', 'dns']);
  expect(unsupportedLabel('host_config:ShmSize')).toBe('host setting ShmSize (not editable here)');
  expect(parseConfiguration({ ...payload(), unsupported: ['host_config:Bad-Key'] }, target)).toBeNull();
  expect(parseConfiguration({ ...payload(), unsupported: [`host_config:${'a'.repeat(65)}`] }, target)).toBeNull();
});

// The Go protocol's caps (internal/agent/protocol): MaxLabels, MaxDeploymentEnvEntries,
// MaxDeploymentPorts, MaxUnsupported + configurationCodes + 2*MaxListEntries.
it('holds the protocol caps: the cap passes, one more is refused', () => {
  const labels = (n: number) => Object.fromEntries(Array.from({ length: n }, (_, i) => [`k${i}`, 'v']));
  const env = (n: number) => Array.from({ length: n }, (_, i) => ({ name: `E${i}`, value: '' }));
  const ports = (n: number) => Array.from({ length: n }, (_, i) => ({ host: 0, container: i + 1, protocol: 'tcp' }));
  const codes = (n: number) => Array.from({ length: n }, (_, i) => `host_config:K${i}`);
  for (const [key, make, cap] of [['labels', labels, 32], ['env', env, 128], ['ports', ports, 64], ['unsupported', codes, 107]] as const) {
    expect(parseConfiguration({ ...payload(), [key]: make(cap) }, target), `${key} ${cap}`).not.toBeNull();
    expect(parseConfiguration({ ...payload(), [key]: make(cap + 1) }, target), `${key} ${cap + 1}`).toBeNull();
  }
});
