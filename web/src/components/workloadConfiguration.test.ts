import { expect, it } from 'vitest';
import { canDestroy, canOperate } from '../tenant';
import { diffWorkload, parseWorkloadConfiguration, toWorkloadSpec, workloadUnsupportedLabel } from './workloadConfiguration';

const target = { namespace: 'default', kind: 'deployment', name: 'web' };
const payload = (): Record<string, any> => ({
  target: { ...target }, observed_at: '2026-09-29T12:00:00Z', resource_version: '42', replicas: 3, paused: false, strategy: 'RollingUpdate',
  containers: [{
    name: 'web', image: 'nginx:1', image_id: '', command: [], args: ['-g'],
    env: [{ name: 'A', value: '1' }, { name: 'B', value: '', secret_ref: 's/k' }, { name: 'C', value: '', config_map_ref: 'c/k' }],
    resources: { cpu_request: '100m', cpu_limit: '', memory_request: '', memory_limit: '1Gi' },
  }],
  init_containers: [], env_from: ['secret/creds'], managed: false, unsupported: ['env_field_ref', 'list_truncated:ports'],
});

it('round-trips a well-formed read', () => {
  const c = parseWorkloadConfiguration(payload(), target)!;
  expect(c.replicas).toBe(3);
  expect(c.containers[0]?.env).toEqual([{ name: 'A', value: '1' }, { name: 'B', secret_ref: 's/k' }, { name: 'C', config_map_ref: 'c/k' }]);
  const spec = toWorkloadSpec(c);
  expect(spec).not.toHaveProperty('observed_at');
  expect(spec).not.toHaveProperty('managed');
  expect(spec).not.toHaveProperty('target');
  expect(spec.resource_version).toBe('42');
  expect(spec.unsupported).toEqual(['env_field_ref', 'list_truncated:ports']);
  expect(diffWorkload(spec, toWorkloadSpec(parseWorkloadConfiguration(payload(), target)!))).toEqual([]);
});

it('accepts a daemonset without replicas', () => {
  const p = payload(); delete p.replicas; p.target.kind = 'daemonset';
  const c = parseWorkloadConfiguration(p, { ...target, kind: 'daemonset' });
  expect(c).not.toBeNull();
  expect(c).not.toHaveProperty('replicas');
});

it('refuses a wrong target and wrong shapes', () => {
  expect(parseWorkloadConfiguration(payload(), { ...target, name: 'other' })).toBeNull();
  expect(parseWorkloadConfiguration(payload(), { ...target, namespace: 'x' })).toBeNull();
  expect(parseWorkloadConfiguration(null, target)).toBeNull();
  const cases: ((p: Record<string, any>) => void)[] = [
    (p) => { p.replicas = '3'; }, (p) => { p.replicas = 1001; }, (p) => { p.replicas = 1.5; },
    (p) => { p.observed_at = 'nope'; }, (p) => { p.paused = 'no'; }, (p) => { p.strategy = 'Bogus'; },
    (p) => { p.containers = []; }, (p) => { p.containers[0].name = 'Web'; }, (p) => { p.containers[0].args = [1]; },
    (p) => { p.containers[0].resources.cpu_limit = 5; }, (p) => { p.containers[0].env = 'x'; },
    (p) => { p.env_from = ['secret/Bad']; }, (p) => { p.env_from = ['x', 'x']; },
    (p) => { p.unsupported = ['made_up']; }, (p) => { p.unsupported = ['env_field_ref', 'env_field_ref']; },
    (p) => { p.init_containers = [structuredClone(p.containers[0])]; },
    (p) => { p.containers[0].env = [{ name: 'A', value: '1', secret_ref: 's/k' }]; },
    (p) => { p.containers[0].env = [{ name: 'A', secret_ref: 's/k', config_map_ref: 'c/k' }]; },
  ];
  for (const [i, mutate] of cases.entries()) {
    const p = payload(); mutate(p);
    expect(parseWorkloadConfiguration(p, target), `case ${i}`).toBeNull();
  }
});

it('labels workload and pattern unsupported codes', () => {
  expect(workloadUnsupportedLabel('env_field_ref')).not.toBe('');
  expect(workloadUnsupportedLabel('list_truncated:ports')).not.toBe('');
  expect(workloadUnsupportedLabel('made_up')).toBe('');
});

it('diffs by path and never by value', () => {
  const a = toWorkloadSpec(parseWorkloadConfiguration(payload(), target)!);
  const b = structuredClone(a);
  b.replicas = 5; b.containers[0]!.image = 'nginx:2'; b.containers[0]!.env[0]!.value = 'secret';
  b.containers[0]!.resources.memory_limit = '2Gi';
  const d = diffWorkload(a, b);
  expect(d).toEqual(['replicas', 'containers.web.image', 'containers.web.env', 'containers.web.resources']);
  expect(JSON.stringify(d)).not.toContain('secret');
  b.containers.push({ ...b.containers[0]!, name: 'side' });
  expect(diffWorkload(a, b)).toContain('containers.side');
});

it('mirrors the server roles', () => {
  for (const [role, op, destroy] of [['organization_admin', true, true], ['environment_admin', true, true], ['operator', true, false], ['developer', false, false], ['read_only', false, false], [undefined, false, false]] as const) {
    expect(canOperate(role), String(role)).toBe(op);
    expect(canDestroy(role), String(role)).toBe(destroy);
  }
});
