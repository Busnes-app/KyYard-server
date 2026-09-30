import { expect, it } from 'vitest';
import { containerPath, matchRoute, runPath, workloadPath } from './router';

it('maps paths to routes and rejects unsafe segments', () => {
  expect(matchRoute('/')).toEqual({ name: 'dashboard' });
  expect(matchRoute('/backup')).toEqual({ name: 'backup' });
  expect(matchRoute('/organizations/org_initial')).toEqual({ name: 'organization', org: 'org_initial' });
  expect(matchRoute('/organizations/org_initial/members')).toEqual({ name: 'members', org: 'org_initial' });
  expect(matchRoute('/organizations/org_initial/audit')).toEqual({ name: 'audit', org: 'org_initial' });
  expect(matchRoute('/organizations/a/environments/env-1')).toEqual({ name: 'environment', org: 'a', env: 'env-1' });
  expect(matchRoute('/organizations/a/endpoints/ep_1')).toEqual({ name: 'endpoint', org: 'a', endpoint: 'ep_1' });
  for (const bad of ['/organizations', '/organizations/a/b', '/organizations/a%2F..%2Fb', '/organizations/' + 'x'.repeat(65), '/nope', '/organizations/%', '/%E0%A4%A', '/organizations/..', '/organizations/.', '/organizations/a/environments/..']) {
    expect(matchRoute(bad).name).toBe('notfound');
  }
});

it('routes a container page and refuses IDs that are not 64 hex', () => {
  const id = 'a'.repeat(64);
  expect(matchRoute(`/organizations/a/endpoints/ep_1/containers/${id}`)).toEqual({ name: 'container', org: 'a', endpoint: 'ep_1', container: id });
  for (const bad of ['..', 'x%2Fy', 'A'.repeat(64), 'a'.repeat(63), 'a'.repeat(65), 'g'.repeat(64)]) {
    expect(matchRoute(`/organizations/a/endpoints/ep_1/containers/${bad}`).name).toBe('notfound');
  }
  expect(matchRoute('/organizations/a/endpoints/ep_1/containers/new')).toEqual({ name: 'container-new', org: 'a', endpoint: 'ep_1' });
  expect(runPath('a', 'ep 1')).toBe('/organizations/a/endpoints/ep%201/containers/new');
  expect(containerPath('a', 'ep 1', id)).toBe(`/organizations/a/endpoints/ep%201/containers/${id}`);
  expect(containerPath('a', 'ep_1', id, 'logs')).toBe(`/organizations/a/endpoints/ep_1/containers/${id}?tab=logs`);
  const escaped = containerPath('a', 'ep_1', '../x/y', 'a&b');
  expect(escaped).toBe('/organizations/a/endpoints/ep_1/containers/..%2Fx%2Fy?tab=a%26b');
  expect(matchRoute(escaped.split('?')[0]).name).toBe('notfound');
});

it('reads a search parameter and follows in-app navigation', async () => {
  const { renderHook, act } = await import('@testing-library/react');
  const { useSearchParam, navigate } = await import('./router');
  const hook = renderHook(() => useSearchParam('tab'));
  expect(hook.result.current).toBe('');
  act(() => navigate('/organizations/a/endpoints/ep_1/containers/' + 'a'.repeat(64) + '?tab=logs'));
  expect(hook.result.current).toBe('logs');
  const entries = window.history.length;
  act(() => navigate(window.location.pathname + '?tab=logs'));
  expect(window.history.length).toBe(entries);
});

it('routes a workload page on DNS-label grammar and encodes its path', () => {
  const base = '/organizations/a/endpoints/ep_1/workloads';
  expect(matchRoute(`${base}/default/deployment/web`)).toEqual({ name: 'workload', org: 'a', endpoint: 'ep_1', namespace: 'default', kind: 'deployment', workload: 'web' });
  expect(matchRoute(`${base}/kube-x/statefulset/db-0`).name).toBe('workload');
  expect(matchRoute(`${base}/default/daemonset/agent`).name).toBe('workload');
  for (const bad of ['Default/deployment/web', '../deployment/web', 'default/deployment/..', 'default/pod/web', 'default/Deployment/web', `default/deployment/${'a'.repeat(64)}`, 'default/deployment', 'default/deployment/web/x', '-a/deployment/web', 'default/deployment/web-']) {
    expect(matchRoute(`${base}/${bad}`).name).toBe('notfound');
  }
  expect(matchRoute(`${base}/default/deployment/${'a'.repeat(63)}`).name).toBe('workload');
  expect(workloadPath('a', 'ep 1', 'default', 'deployment', 'web')).toBe('/organizations/a/endpoints/ep%201/workloads/default/deployment/web');
  expect(workloadPath('a', 'ep_1', 'default', 'deployment', 'web', 'logs')).toBe(`${base}/default/deployment/web?tab=logs`);
  const escaped = workloadPath('a', 'ep_1', 'x/y', 'deployment', 'web');
  expect(matchRoute(escaped).name).toBe('notfound');
});
