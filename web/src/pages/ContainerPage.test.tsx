import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ContainerPage } from './ContainerPage';
vi.mock('../components/ContainerTerminal', () => ({ ContainerTerminal: () => <p>terminal stub</p> }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, '', '/'); });

const id = 'c'.repeat(64);
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: {}, fingerprint: '', capabilities: ['container.inspect'], alerts: [], created_at: '' };
const container = (over: object = {}) => ({ id, name: 'web', image: 'nginx:1', image_id: 'sha256:1', state: 'running', status: 'Up', created_at: '2026-09-29T08:00:00Z', started_at: '2026-09-29T09:00:00Z', health: 'healthy', restart_policy: 'always', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: { tier: 'web' }, networks: ['bridge'], network_attachments: [{ name: 'bridge', ip: '172.17.0.2' }], mounts: [{ kind: 'volume', source: 'data', target: '/data', read_only: false }], ...over });
function stub(role = 'organization_admin', containers: object[] = [container()], opts: { ep?: object; inventoryStatus?: number; rollups?: object[]; commands?: object[] } = {}) {
  const now = '2026-09-29T10:00:00Z';
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Team', role }]);
    if (url.endsWith('/inventory') && opts.inventoryStatus) return json({}, opts.inventoryStatus);
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '29', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [] } });
    if (url.includes('/commands') && opts.commands) return json(opts.commands);
    if (url.includes('/commands')) return json([{ id: 'cmd1', action: 'container.restart', outcome: 'succeeded', container_id: id, created_at: now }]);
    if (url.endsWith('/rollups?hours=24') && opts.rollups) return json(opts.rollups);
    if (url.endsWith('/rollups?hours=24')) return json([{ container_id: id, hour: now, samples: 60, cpu_avg: 1.5, cpu_max: 3, memory_avg: 1048576, memory_max: 2097152, rx_bytes: 10, tx_bytes: 20, pids_max: 4, restart_count: 0 }]);
    if (url.endsWith('/inspection')) return json({ target: { container_id: id, image_id: 'sha256:1', created_unix: Date.parse('2026-09-29T08:00:00Z') / 1000 }, observed_at: now, state: 'running', health: 'healthy', restart_count: 0, image_platform: { os: 'linux', architecture: 'amd64' }, restart_policy: 'always', restart_retries: 0, ports: [], mounts: { bind: 0, volume: 1, tmpfs: 0, other: 0, read_only: 0 }, network_mode: 'bridge', network_count: 1, privileged: false, read_only_rootfs: false, auto_remove: false, configuration_verified: true, unsupported: [] });
    return json({ ...endpoint, ...opts.ep });
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

it('shows overview facts with live uptime, IP, health and links back to the host', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  stub();
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByRole('heading', { level: 1, name: /web/ })).toBeTruthy();
  const facts = screen.getByRole('region', { name: 'Overview' });
  expect(facts.textContent).toContain('1h 0m');
  expect(facts.textContent).toContain('172.17.0.2');
  expect(facts.textContent).toContain('always');
  expect(facts.textContent).toContain('tier=web');
  expect(facts.textContent).toContain('/data');
  expect(screen.getByText('healthy')).toBeTruthy();
  await act(async () => { vi.advanceTimersByTime(60_000); });
  expect(facts.textContent).toContain('1h 1m');
  expect(screen.getByRole('link', { name: 'host-1' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_1');
});

it('opens the tab named in the URL and puts tab changes in the URL', async () => {
  stub();
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=logs`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByRole('button', { name: 'Load logs' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Activity' }));
  expect(window.location.search).toBe('?tab=activity');
  expect(await screen.findByText(/container.restart/)).toBeTruthy();
});

it.each([['organization_admin', true], ['operator', false]])('offers the Terminal tab to %s: %s', async (role, want) => {
  stub(role);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  await screen.findByRole('heading', { level: 1, name: /web/ });
  await act(async () => {});
  expect(within(screen.getByRole('navigation', { name: 'Container sections' })).queryByRole('button', { name: 'Terminal' }) !== null).toBe(want);
});

it('says when the container is no longer reported and disables actions', async () => {
  stub('organization_admin', []);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/no longer reported/)).toBeTruthy();
  expect(screen.queryByLabelText('Actions for web')).toBeNull();
  expect(screen.getAllByRole('link', { name: 'host-1' }).length).toBe(2);
});

it('shows the redacted inspection under Configuration', async () => {
  stub();
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
  expect(screen.getByRole('region', { name: 'Configuration' }).textContent).toContain('bridge');
});

const hour = (over: object) => ({ container_id: id, hour: '2026-09-29T09:00:00Z', samples: 1, cpu_avg: 0, cpu_max: 0, memory_avg: 0, memory_max: 0, rx_bytes: 0, tx_bytes: 0, pids_max: 0, restart_count: 0, ...over });

it('aggregates hourly rollups in hour order, network as counter rises', async () => {
  stub('organization_admin', [container()], { rollups: [
    hour({ hour: '2026-09-29T10:00:00Z', samples: 30, cpu_avg: 4, cpu_max: 9, memory_avg: 1048576, memory_max: 3145728, rx_bytes: 3072, tx_bytes: 6144, pids_max: 7, restart_count: 2 }),
    hour({ hour: '2026-09-29T09:00:00Z', samples: 10, cpu_avg: 0, cpu_max: 2, memory_avg: 5242880, memory_max: 2097152, rx_bytes: 1024, tx_bytes: 2048, pids_max: 3, restart_count: 1 }),
  ] });
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  const facts = (await screen.findByText('Last 24 hours')).parentElement!;
  await screen.findByText(/avg 3\.0%/);
  expect(facts.textContent).toContain('peak 9.0%');
  expect(facts.textContent).toContain('avg 2 MiB');
  expect(facts.textContent).toContain('peak 3 MiB');
  expect(facts.textContent).toContain('rx 2048 B');
  expect(facts.textContent).toContain('tx 4096 B');
  expect(facts.textContent).toContain('peak 7');
  expect(facts.textContent).toContain('Restart count2');
});

it('skips unmeasured CPU and restarts and counts no traffic across a counter reset', async () => {
  stub('organization_admin', [container()], { rollups: [
    hour({ hour: '2026-09-29T10:00:00Z', samples: 10, cpu_avg: 4, cpu_max: 6, rx_bytes: 100, tx_bytes: 100, restart_count: 0 }),
    hour({ hour: '2026-09-29T08:00:00Z', samples: 20, cpu_avg: -1, cpu_max: -1, rx_bytes: 5000, tx_bytes: 5000, restart_count: -1 }),
    hour({ hour: '2026-09-29T09:00:00Z', samples: 10, cpu_avg: 2, cpu_max: 5, rx_bytes: 6000, tx_bytes: 5500, restart_count: -1 }),
  ] });
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  const facts = (await screen.findByText('Last 24 hours')).parentElement!;
  await screen.findByText(/avg 3\.0%/);
  expect(facts.textContent).toContain('peak 6.0%');
  expect(facts.textContent).toContain('rx 1000 B');
  expect(facts.textContent).toContain('tx 500 B');
  expect(facts.textContent).toContain('Restart count0');
});

it('shows no CPU figure when no hour measured it', async () => {
  stub('organization_admin', [container()], { rollups: [hour({ samples: 5, cpu_avg: -1, cpu_max: -1, rx_bytes: 900, restart_count: -1 })] });
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  const facts = (await screen.findByText('Last 24 hours')).parentElement!;
  await screen.findByText(/rx 0 B/);
  expect(facts.textContent).toContain('CPU—');
  expect(facts.textContent).toContain('Restart count—');
});

it('asks for an agent upgrade when the host lacks container.inspect', async () => {
  stub('organization_admin', [container()], { ep: { capabilities: [] } });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/Upgrade the host agent/)).toBeTruthy();
});

it('shows the fixed notice instead of a terminal for a stopped container', async () => {
  stub('organization_admin', [container({ state: 'exited' })]);
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=terminal`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/terminal needs a running container/)).toBeTruthy();
  expect(screen.queryByText('terminal stub')).toBeNull();
});

it('falls back to overview when a role that cannot exec asks for the terminal tab', async () => {
  stub('operator');
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=terminal`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByRole('region', { name: 'Overview' })).toBeTruthy();
  expect(screen.queryByRole('region', { name: 'Terminal' })).toBeNull();
});

it('stops polling the inventory after a denial', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const fetcher = stub('organization_admin', [container()], { inventoryStatus: 403 });
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  await act(async () => {});
  const inventoryCalls = () => fetcher.mock.calls.filter(([u]) => String(u).endsWith('/inventory')).length;
  const before = inventoryCalls();
  expect(before).toBeGreaterThan(0);
  await act(async () => { vi.advanceTimersByTime(60_000); });
  expect(inventoryCalls()).toBe(before);
});

it('renders activity detail as inert display text', async () => {
  stub('organization_admin', [container()], { commands: [{ id: 'k', action: 'container.stop', outcome: 'failed', detail: 'bad\u0007code', created_at: '2026-09-29T10:00:00Z' }] });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=activity`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect((await screen.findByText(/container.stop/)).textContent).not.toContain('\u0007');
});
