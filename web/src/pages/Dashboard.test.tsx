import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Dashboard } from './Dashboard';
const json = (body: unknown) => new Response(JSON.stringify(body));
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const hosts = [
  { id: 'old', name: 'Local Docker', runtime: 'docker', state: 'offline', facts: {} },
  { id: 'cluster', name: 'K80 & K81', runtime: 'kubernetes', state: 'active', facts: {} },
  { id: 'docker', name: 'docker.urlxl.us', runtime: 'docker', state: 'active', facts: {} },
];
function fleet(role = 'organization_admin', received = new Date().toISOString(), denied = false) {
  const fetcher = vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', role }]) : url.includes('/inventory') ? denied ? new Response('{}', { status: 403 }) : json({ received_at: received, snapshot: { engine: { cpus: 16, memory_bytes: 16 * 2 ** 30 }, containers: [{ state: 'running' }, { state: 'exited' }], kubernetes: { nodes: [{ ready: true }, { ready: false }], pods: [{}, {}, {}], workloads: [{ ready: 1, desired: 1 }, { ready: 0, desired: 1 }] } } }) : json(hosts));
  vi.stubGlobal('fetch', fetcher); return fetcher;
}
it('lands on native endpoint summaries and direct run links, without stale local containers', async () => {
  const fetcher = fleet(); render(<Dashboard />);
  expect(await screen.findByText('16.0 GiB')).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Home' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Run a container on Kubernetes' }).getAttribute('href')).toBe('/organizations/a/endpoints/cluster/workloads/new');
  expect(screen.getByRole('link', { name: 'Run a container' }).getAttribute('href')).toBe('/organizations/a/endpoints/docker/containers/new');
  expect(fetcher.mock.calls.some(([url]) => url.includes('/old/inventory'))).toBe(false);
  expect(screen.queryByRole('table')).toBeNull();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'K80' } });
  expect(screen.queryByRole('link', { name: 'docker.urlxl.us' })).toBeNull();
  expect(screen.getByRole('link', { name: 'K80 & K81' })).toBeTruthy();
});
it('hides run actions from an operator', async () => {
  fleet('operator'); render(<Dashboard />); await screen.findByText('16.0 GiB');
  expect(screen.queryByRole('link', { name: /Run a container/ })).toBeNull();
});
it('reports stale inventory without claiming current running counts', async () => {
  fleet('organization_admin', new Date(Date.now() - 240000).toISOString()); render(<Dashboard />);
  expect((await screen.findAllByText(/Inventory is stale/)).length).toBe(2);
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
  vi.stubGlobal('fetch', fetcher); render(<Dashboard />); await screen.findByText('host-0');
  fireEvent.click(screen.getByRole('button', { name: 'Next endpoints' }));
  await screen.findByText('No endpoints on this page');
  expect(fetcher.mock.calls.some(([url]) => url.endsWith('offset=20&limit=20'))).toBe(true);
});
