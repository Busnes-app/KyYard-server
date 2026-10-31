import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { Dashboard } from './Dashboard';
const json = (body: unknown) => new Response(JSON.stringify(body));
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const hosts = [
  { id: 'old', name: 'Local Docker', runtime: 'docker', state: 'offline', facts: {} },
  { id: 'cluster', name: 'K80 & K81', runtime: 'kubernetes', state: 'active', facts: {} },
  { id: 'docker', name: 'docker.example.com', runtime: 'docker', state: 'active', facts: {} },
];
function fleet(role = 'organization_admin', received = new Date().toISOString(), denied = false) {
  const fetcher = vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', role }]) : url.includes('/inventory') ? denied ? new Response('{}', { status: 403 }) : json({ received_at: received, snapshot: { engine: { cpus: 16, memory_bytes: 16 * 2 ** 30 }, containers: [{ id: 'c1', name: 'web', state: 'running' }, { id: 'c2', name: 'job', state: 'exited', status: 'Exited (1)' }], kubernetes: { nodes: [{ name: 'n1', ready: true }, { name: 'n2', ready: false }], pods: [{}, {}, {}], workloads: [{ ready: 1, desired: 1 }, { ready: 0, desired: 1 }] } } }) : url.includes('/commands') ? json([]) : json(hosts));
  vi.stubGlobal('fetch', fetcher); return fetcher;
}
it('lands on native endpoint summaries and direct run links, without stale local containers', async () => {
  const fetcher = fleet(); render(<Dashboard />);
  expect(await screen.findByText('16.0 GiB')).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Home' })).toBeTruthy();
  expect(screen.getByText('1 exited')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Run a container on Kubernetes' }).getAttribute('href')).toBe('/organizations/a/endpoints/cluster/workloads/new');
  expect(screen.getByRole('link', { name: 'Run a container' }).getAttribute('href')).toBe('/organizations/a/endpoints/docker/containers/new');
  expect(fetcher.mock.calls.some(([url]) => url.includes('/old/inventory'))).toBe(false);
  expect(screen.queryByRole('columnheader', { name: 'Container' })).toBeNull();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'K80' } });
  expect(screen.queryByRole('link', { name: 'docker.example.com' })).toBeNull();
  expect(screen.getByRole('link', { name: 'K80 & K81' })).toBeTruthy();
});
it('hides run actions from an operator', async () => {
  fleet('operator'); render(<Dashboard />); await screen.findByText('16.0 GiB');
  expect(screen.queryByRole('link', { name: /Run a container/ })).toBeNull();
});
it('reports stale inventory without claiming current running counts', async () => {
  fleet('organization_admin', new Date(Date.now() - 240000).toISOString()); render(<Dashboard />);
  const attention = await screen.findByRole('region', { name: 'Needs attention (3)' });
  expect(within(attention).getAllByText('Stale').length).toBe(2);
  expect(within(attention).getAllByRole('listitem')[0].textContent).toContain('Disconnected');
  expect(within(screen.getByRole('table')).getAllByText('Stale').length).toBe(2);
  expect(screen.queryByText('16.0 GiB')).toBeNull();
});
it('distinguishes denied organization access from an empty fleet', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 403 })));
  render(<Dashboard />); await screen.findByRole('alert');
  expect(screen.queryByText(/No endpoints connected/)).toBeNull();
});
it.each([false, true])('polls active inventory and stops denied reads (%s)', async (denied) => {
  vi.useFakeTimers(); const fetcher = fleet('operator', new Date().toISOString(), denied);
  await act(async () => { render(<Dashboard />); });
  const reads = () => fetcher.mock.calls.filter(([url]) => url.includes('/inventory')).length;
  expect(reads()).toBe(2);
  await act(async () => { vi.advanceTimersByTime(30000); });
  expect(reads()).toBe(denied ? 2 : 4);
});
it('keeps endpoint discovery bounded at 20 per page', async () => {
  const fetcher = vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a' }]) : json(url.includes('offset=20') ? [] : Array.from({ length: 20 }, (_, i) => ({ id: `e${i}`, name: `host-${i}`, state: 'offline', runtime: 'docker', facts: {} }))));
  vi.stubGlobal('fetch', fetcher); render(<Dashboard />); await screen.findAllByText('host-0');
  fireEvent.click(screen.getByRole('button', { name: 'Next endpoints' }));
  await screen.findByText('No endpoints on this page');
  expect(fetcher.mock.calls.some(([url]) => url.endsWith('offset=20&limit=20'))).toBe(true);
});

it('shows scope and role-aware navigation, and clears a search without losing endpoints', async () => {
  fleet('operator'); render(<Dashboard />);
  await screen.findByText('16.0 GiB');
  expect(screen.getByText('Your role')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'View environments' })).toBeTruthy();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'no such host' } });
  expect(screen.getByText(/No endpoints match/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Clear search' }));
  expect(screen.getByRole('link', { name: 'docker.example.com' })).toBeTruthy();
});
// Before any organization exists, a platform administrator is pointed at creating one; everyone
// else is told to ask for access.
it('points a platform administrator without an organization at Administration', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json([])));
  render(<Dashboard platformAdmin />);
  expect((await screen.findByRole('link', { name: 'create one in Settings → Administration' })).getAttribute('href')).toBe('/settings/administration');
  cleanup();
  render(<Dashboard />);
  expect(await screen.findByText('No host access yet. Ask an administrator to add your account.')).toBeTruthy();
});

it('leads with what needs attention, worst first, and ends with recent commands', async () => {
  const now = new Date().toISOString();
  const inventory = (containers: object[]) => json({ received_at: now, snapshot: { engine: { cpus: 1, memory_bytes: 2 ** 30 }, containers } });
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', role: 'operator' }])
    : url.includes('/calm/inventory') ? inventory([{ id: 'c9', name: 'cache', state: 'running', health: 'healthy', status: 'Up' }])
    : url.includes('/busy/inventory') ? inventory([
      { id: 'c1', name: 'web', state: 'running', health: 'starting', status: 'Up 1 minute (health: starting)' },
      { id: 'c2', name: 'db', state: 'running', health: 'unhealthy', status: 'Up 5 hours (unhealthy)' },
      { id: 'c3', name: 'idle', state: 'created', status: 'Created' }])
    : url.includes('/busy/commands') ? json([{ id: 'k1', action: 'container.restart', outcome: 'failed', container_id: 'c2', created_at: now }, { id: 'k2', action: 'something.new', outcome: 'odd', created_at: now }])
    : url.includes('/commands') ? json([])
    : json([{ id: 'calm', name: 'Alpha', runtime: 'docker', state: 'active', facts: {} }, { id: 'busy', name: 'Zulu', runtime: 'docker', state: 'active', facts: {} }])));
  render(<Dashboard />);
  const attention = await screen.findByRole('region', { name: 'Needs attention (2)' });
  const items = within(attention).getAllByRole('listitem');
  expect(items.map((li) => li.textContent)).toEqual(['Unhealthydb' + 'Zulu · Up 5 hours (unhealthy)', 'Startingweb' + 'Zulu · Up 1 minute (health: starting)']);
  expect(within(items[0]).getByText('Unhealthy').getAttribute('data-ky-status')).toBe('danger');
  expect(within(items[0]).getByRole('link', { name: 'db' }).getAttribute('href')).toBe('/organizations/a/endpoints/busy/containers/c2');
  expect(within(attention).queryByText('idle')).toBeNull();
  // The endpoint with problems sorts ahead of a healthy one whose name comes first.
  const rows = within(screen.getByRole('table')).getAllByRole('row').slice(1);
  expect(rows.map((r) => within(r).getAllByRole('link')[0].textContent)).toEqual(['Zulu', 'Alpha']);
  const activity = within(screen.getByRole('region', { name: 'Recent activity' })).getAllByRole('listitem');
  expect(within(activity[0]).getByRole('link').textContent).toBe('Restart db');
  expect(within(activity[0]).getByText('Failed').getAttribute('data-ky-status')).toBe('danger');
  // Unknown codes never render raw.
  expect(activity[1].textContent).toContain('Command');
  expect(activity[1].textContent).toContain('Unrecognised');
  expect(activity[1].textContent).not.toMatch(/something\.new|odd/);
});
