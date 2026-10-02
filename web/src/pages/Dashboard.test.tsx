import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { Dashboard } from './Dashboard';
const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
it('puts container inventory on the home page and links to real endpoint operations', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'web', image: 'nginx:1', state: 'running', ports: [] }] } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  expect(await screen.findByText('web')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Logs for web' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Stop web' })).toBeTruthy();
  expect(screen.queryByRole('combobox')).toBeNull();
  expect(screen.queryByText(/Directory|Feature 0|Pluggable/)).toBeNull();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'missing' } });
  expect(screen.getByText('No matching containers on this host.')).toBeTruthy();
});
it('does not invent an empty fleet when organization access is denied', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 403 })));
  render(<Dashboard />);
  expect(await screen.findByRole('alert')).toBeTruthy();
  expect(screen.queryByText(/No endpoints here/)).toBeNull();
});

it('reports unavailable Docker instead of an empty host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { engine: { version: '' }, containers: [] } }) : json([{ id: 'e', name: 'Local Docker', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  expect(await screen.findByText(/Docker is unavailable/)).toBeTruthy();
  expect(screen.queryByText('No containers on this host.')).toBeNull();
});

it('names the target host when identical container names occur on different hosts', async () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'web', image: 'nginx:1', state: 'running', ports: [] }] } }) : json(['First host', 'Second host'].map((name, i) => ({ id: `e${i}`, name, state: 'active', runtime: 'docker', facts: {} })))));
  render(<Dashboard />);
  await screen.findAllByText('web');
  const hostSelect = screen.queryByRole('combobox', { name: 'Endpoint' });
  if (hostSelect) fireEvent.change(hostSelect, { target: { value: 'e1' } });
  const buttons = await screen.findAllByRole('button', { name: 'Stop web' });
  fireEvent.click(buttons[buttons.length - 1]);
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Second host'));
});

it('pages a large inventory and searches containers beyond the current page', async () => {
  let count = 61;
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: Array.from({ length: count }, (_, i) => ({ id: `c${i}`, name: `container-${i}`, image: 'nginx:1', state: 'running', ports: [] })) } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  await screen.findByText('container-0');
  expect(screen.getAllByRole('row')).toHaveLength(26);
  expect(screen.queryByText('container-25')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('container-25')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('51–61 of 61')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Next page' }).hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'container-0' } });
  expect(screen.getByText('container-0')).toBeTruthy();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: '' } });
  expect(screen.getByText('1–25 of 61')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  count = 2;
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByText('container-0');
  expect(screen.queryByRole('navigation', { name: 'Pagination' })).toBeNull();
});
it.each([['organization_admin', true], ['environment_admin', false], ['operator', false], ['developer', false], ['read_only', false]])('offers Terminal to %s: %s', async (role, want) => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'web', image: 'nginx:1', state: 'running', ports: [] }] } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  await screen.findByRole('group', { name: 'Actions for web' });
  await waitFor(() => expect(screen.queryByRole('link', { name: 'Terminal for web' }) !== null).toBe(want));
});

it('lists uptime and IP and links container names to their page', async () => {
  const id = 'c'.repeat(64);
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id, name: 'web', image: 'nginx:1', state: 'running', started_at: '2026-09-29T07:30:00Z', health: 'healthy', ports: [], networks: ['bridge'], network_attachments: [{ name: 'bridge', ip: '172.17.0.5' }] }] } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  const link = await screen.findByRole('link', { name: 'web' });
  expect(link.getAttribute('href')).toBe(`/organizations/a/endpoints/e/containers/${id}`);
  const table = screen.getByRole('table');
  expect(within(table).getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Container', 'Status', 'Uptime', 'IP', 'Ports', 'Actions']);
  expect(table.textContent).toContain('172.17.0.5');
  expect(within(table).getByText('healthy')).toBeTruthy();
});

const mixedHosts = [
  { id: 'old', name: 'Local Docker', runtime: 'docker', state: 'offline', facts: {} },
  { id: 'cluster', name: 'K80 & K81', runtime: 'kubernetes', state: 'active', facts: {} },
  { id: 'docker', name: 'docker.urlxl.us', runtime: 'docker', state: 'active', facts: {} },
];
const clusterReport = { received_at: new Date().toISOString(), snapshot: { containers: [], kubernetes: {
  nodes: [{ name: 'k80', ready: true }, { name: 'k81', ready: true }], pods: [{ name: 'kyyard-pod' }],
  workloads: [{ namespace: 'ky', name: 'kyyard', kind: 'Deployment', ready: 1, desired: 1, images: ['ghcr.io/busnes-app/kyyard:latest'] }],
} } };
function mixedFleet() {
  const fetcher = vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'organization_admin' }]) : url.includes('/cluster/inventory') ? json(clusterReport) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'old-web', image: 'nginx', state: 'running', ports: [] }] } }) : json(mixedHosts));
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
it('prefers the active cluster and switches between native workloads and Docker containers', async () => {
  const fetcher = mixedFleet(); render(<Dashboard />);
  const workload = await screen.findByRole('link', { name: 'ky/kyyard' });
  expect(workload.getAttribute('href')).toBe('/organizations/a/endpoints/cluster/workloads/ky/deployment/kyyard');
  expect(screen.getByText(/2 of 2 nodes ready · 1 pods/)).toBeTruthy();
  expect(screen.queryByText('No containers on this host.')).toBeNull();
  expect(screen.queryByRole('group', { name: /Actions/ })).toBeNull();
  expect(fetcher.mock.calls.some(([url]) => url.includes('/old/inventory'))).toBe(false);
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'missing' } });
  expect(screen.getByText('No matching workloads on this cluster.')).toBeTruthy();
  fireEvent.change(screen.getByRole('combobox', { name: 'Endpoint' }), { target: { value: 'docker' } });
  expect(await screen.findByRole('button', { name: 'Stop old-web' })).toBeTruthy();
});
it('collapses historical local inventory and disables its mutations without deleting it', async () => {
  mixedFleet(); render(<Dashboard />); await screen.findByRole('link', { name: 'ky/kyyard' });
  fireEvent.change(screen.getByRole('combobox', { name: 'Endpoint' }), { target: { value: 'old' } });
  expect(screen.getByText(/Its saved inventory is historical/)).toBeTruthy();
  const summary = screen.getByText('Show last reported inventory');
  expect(summary.closest('details')?.open).toBe(false);
  fireEvent.click(summary);
  expect((await screen.findByRole('button', { name: 'Stop old-web', hidden: true })).hasAttribute('disabled')).toBe(true);
  expect(screen.getByText(/last reported state, not live/)).toBeTruthy();
});
it('distinguishes an unreported cluster from an empty Docker host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [] } }) : json([mixedHosts[1]])));
  render(<Dashboard />);
  expect(await screen.findByText('The agent has not reported the cluster yet.')).toBeTruthy();
  expect(screen.queryByText('No containers on this host.')).toBeNull();
});
it('disables lifecycle actions on stale active-host inventory', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a' }]) : url.includes('/inventory') ? json({ received_at: new Date(Date.now() - 240000).toISOString(), snapshot: { containers: [{ id: 'c', name: 'stale-web', state: 'running', started_at: '2026-01-01T00:00:00Z', image: 'nginx', ports: [] }] } }) : json([mixedHosts[2]])));
  render(<Dashboard />);
  expect((await screen.findByRole('button', { name: 'Stop stale-web' })).hasAttribute('disabled')).toBe(true);
  expect(screen.getByText(/stale inventory/)).toBeTruthy();
});

it.each([200, 403])('polls endpoint and inventory reads, stopping denied inventory (%s)', async (status) => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a' }]) : url.includes('/inventory') ? status === 403 ? new Response('{}', { status }) : json(clusterReport) : json([mixedHosts[1]]));
  vi.stubGlobal('fetch', fetcher);
  await act(async () => { render(<Dashboard />); });
  const reads = () => fetcher.mock.calls.filter(([url]) => url.includes('/inventory')).length;
  expect(reads()).toBe(1);
  await act(async () => { vi.advanceTimersByTime(30000); });
  expect(reads()).toBe(status === 403 ? 1 : 2);
  expect(fetcher.mock.calls.filter(([url]) => url.includes('/endpoints?')).length).toBe(2);
});
it('bounds Kubernetes workload tables and searches beyond the current page', async () => {
  const report = { ...clusterReport, snapshot: { containers: [], kubernetes: { ...clusterReport.snapshot.kubernetes, workloads: Array.from({ length: 61 }, (_, i) => ({ namespace: 'ky', name: `app-${i}`, kind: 'Deployment', ready: 1, desired: 1, images: ['nginx'] })) } } };
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a' }]) : url.includes('/inventory') ? json(report) : json([mixedHosts[1]])));
  render(<Dashboard />); await screen.findByText('ky/app-0');
  expect(screen.getAllByRole('row')).toHaveLength(26);
  expect(screen.queryByText('ky/app-60')).toBeNull();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'app-60' } });
  expect(screen.getByText('ky/app-60')).toBeTruthy();
  expect(screen.getAllByRole('row')).toHaveLength(2);
});
