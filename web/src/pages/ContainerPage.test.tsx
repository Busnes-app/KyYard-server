import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ContainerPage } from './ContainerPage';
vi.mock('../components/ContainerTerminal', () => ({ ContainerTerminal: () => <p>terminal stub</p> }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, '', '/'); });

const id = 'c'.repeat(64);
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: {}, fingerprint: '', capabilities: ['container.inspect'], alerts: [], created_at: '' };
const container = (over: object = {}) => ({ id, name: 'web', image: 'nginx:1', image_id: 'sha256:1', state: 'running', status: 'Up', created_at: '2026-09-29T08:00:00Z', started_at: '2026-09-29T09:00:00Z', health: 'healthy', restart_policy: 'always', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: { tier: 'web' }, networks: [{ name: 'bridge', ip: '172.17.0.2' }], mounts: [{ kind: 'volume', source: 'data', target: '/data', read_only: false }], ...over });
function stub(role = 'organization_admin', containers: object[] = [container()]) {
  const now = '2026-09-29T10:00:00Z';
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Team', role }]);
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '29', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [] } });
    if (url.includes('/commands')) return json([{ id: 'cmd1', action: 'container.restart', outcome: 'succeeded', container_id: id, created_at: now }]);
    if (url.endsWith('/rollups?hours=24')) return json([{ container_id: id, hour: now, samples: 60, cpu_avg: 1.5, cpu_max: 3, memory_avg: 1048576, memory_max: 2097152, rx_bytes: 10, tx_bytes: 20, pids_max: 4, restart_count: 0 }]);
    if (url.endsWith('/inspection')) return json({ target: { container_id: id, image_id: 'sha256:1', created_unix: Date.parse('2026-09-29T08:00:00Z') / 1000 }, observed_at: now, state: 'running', health: 'healthy', restart_count: 0, image_platform: { os: 'linux', architecture: 'amd64' }, restart_policy: 'always', restart_retries: 0, ports: [], mounts: { bind: 0, volume: 1, tmpfs: 0, other: 0, read_only: 0 }, network_mode: 'bridge', network_count: 1, privileged: false, read_only_rootfs: false, auto_remove: false, configuration_verified: true, unsupported: [] });
    return json(endpoint);
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
  expect(screen.queryByRole('button', { name: /Restart web/ })).toBeNull();
  expect(screen.getAllByRole('link', { name: 'host-1' }).length).toBe(2);
});

it('shows the redacted inspection under Configuration', async () => {
  stub();
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
  expect(screen.getByRole('region', { name: 'Configuration' }).textContent).toContain('bridge');
});
