import { expect, it } from 'vitest';
import { matchRoute } from './router';

it('maps paths to routes and rejects unsafe segments', () => {
  expect(matchRoute('/')).toEqual({ name: 'dashboard' });
  expect(matchRoute('/backup')).toEqual({ name: 'backup' });
  expect(matchRoute('/organizations/org_initial')).toEqual({ name: 'organization', org: 'org_initial' });
  expect(matchRoute('/organizations/org_initial/members')).toEqual({ name: 'members', org: 'org_initial' });
  expect(matchRoute('/organizations/org_initial/audit')).toEqual({ name: 'audit', org: 'org_initial' });
  expect(matchRoute('/organizations/a/environments/env-1')).toEqual({ name: 'environment', org: 'a', env: 'env-1' });
  for (const bad of ['/organizations', '/organizations/a/b', '/organizations/a%2F..%2Fb', '/organizations/' + 'x'.repeat(65), '/nope', '/organizations/%', '/%E0%A4%A', '/organizations/..', '/organizations/.', '/organizations/a/environments/..']) {
    expect(matchRoute(bad).name).toBe('notfound');
  }
});
