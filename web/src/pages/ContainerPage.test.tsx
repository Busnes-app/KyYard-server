import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ContainerPage, ContainerRunPage } from './ContainerPage';
vi.mock('../components/ContainerTerminal', () => ({ ContainerTerminal: () => <p>terminal stub</p> }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, '', '/'); });

const id = 'c'.repeat(64);
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: {}, fingerprint: '', capabilities: ['container.inspect'], alerts: [], created_at: '' };
const container = (over: object = {}) => ({ id, name: 'web', image: 'nginx:1', image_id: 'sha256:1', state: 'running', status: 'Up', created_at: '2026-09-29T08:00:00Z', started_at: '2026-09-29T09:00:00Z', health: 'healthy', restart_policy: 'always', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: { tier: 'web' }, networks: ['bridge'], network_attachments: [{ name: 'bridge', ip: '172.17.0.2' }], mounts: [{ kind: 'volume', source: 'data', target: '/data', read_only: false }], ...over });
function stub(role = 'organization_admin', containers: object[] = [container()], opts: { ep?: object; inventoryStatus?: number; rollups?: object[]; commands?: object[]; applications?: object[]; command?: object; epStatus?: number; orgStatus?: number; configurationStatus?: number; run?: object } = {}) {
  const now = '2026-09-29T10:00:00Z';
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return opts.orgStatus ? json({}, opts.orgStatus) : json([{ id: 'a', name: 'Team', role }]);
    if (url.endsWith('/ep_1/containers') && opts.run) return json(opts.run, 202);
    if (url.endsWith('/inventory') && opts.inventoryStatus) return json({}, opts.inventoryStatus);
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '29', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [] } });
    if (url.endsWith('/recreate')) return json({ id: 'cmd9', action: 'container.recreate', outcome: '' }, 202);
    if (url.endsWith('/commands/cmd9') && opts.command) return json(opts.command);
    if (url.endsWith('/applications')) return json(opts.applications ?? []);
    if (url.endsWith('/configuration')) return opts.configurationStatus ? json({}, opts.configurationStatus) : json(configuration);
    if (url.includes('/commands') && opts.commands) return json(opts.commands);
    if (url.includes('/commands')) return json([{ id: 'cmd1', action: 'container.restart', outcome: 'succeeded', container_id: id, created_at: now }]);
    if (url.endsWith('/rollups?hours=24') && opts.rollups) return json(opts.rollups);
    if (url.endsWith('/rollups?hours=24')) return json([{ container_id: id, hour: now, samples: 60, cpu_avg: 1.5, cpu_max: 3, memory_avg: 1048576, memory_max: 2097152, rx_bytes: 10, tx_bytes: 20, pids_max: 4, restart_count: 0 }]);
    if (url.endsWith('/inspection')) return json({ target: { container_id: id, image_id: 'sha256:1', created_unix: Date.parse('2026-09-29T08:00:00Z') / 1000 }, observed_at: now, state: 'running', health: 'healthy', restart_count: 0, image_platform: { os: 'linux', architecture: 'amd64' }, restart_policy: 'always', restart_retries: 0, ports: [], mounts: { bind: 0, volume: 1, tmpfs: 0, other: 0, read_only: 0 }, network_mode: 'bridge', network_count: 1, privileged: false, read_only_rootfs: false, auto_remove: false, configuration_verified: true, unsupported: [] });
    return json({ ...endpoint, ...opts.ep }, opts.epStatus ?? 200);
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
  // Step 1 s at a time: the interval registers in an effect that may land late under load.
  for (let i = 0; i < 120 && !facts.textContent?.includes('1h 1m'); i++) await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
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

it('shows the redacted inspection under Configuration to a role that cannot configure', async () => {
  stub('operator', [container()], { ep: { capabilities: ['container.inspect', 'container.configure'] } });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
  expect(screen.getByRole('region', { name: 'Configuration' }).textContent).toContain('bridge');
});

const configuration = {
  target: { container_id: id, image_id: 'sha256:1', created_unix: Date.parse('2026-09-29T08:00:00Z') / 1000 }, observed_at: '2026-09-29T10:00:00Z', name: 'web',
  image: { reference: 'nginx:1', digest: '' }, image_id: 'sha256:1', command: [], entrypoint: [], user: '', working_dir: '', hostname: 'box',
  env: [{ name: 'TOKEN', value: 's3cret' }], labels: {}, restart: 'always', restart_retries: 0, ports: [], mounts: [], network_mode: 'bridge', networks: [],
  resources: { nano_cpus: 0, memory_bytes: 0, memory_swap_bytes: 0, pids_limit: 0 }, healthcheck: null, privileged: false, read_only_rootfs: false, init: false, tty: false, stdin_open: false,
  cap_add: [], cap_drop: [], security_opt: [], extra_hosts: [], dns: [], devices: [], log: { driver: 'json-file', options: {} }, stop_signal: '', unsupported: [],
};
const configurable = { ep: { capabilities: ['container.inspect', 'container.configure', 'deployment.pull'] } };

it('shows an organization administrator the editable configuration, values masked', async () => {
  const fetcher = stub('organization_admin', [container()], configurable);
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(((await screen.findByLabelText('Hostname')) as HTMLInputElement).value).toBe('box');
  expect(screen.getByRole('button', { name: 'Reveal value of TOKEN' }).textContent).toBe('••••••');
  expect(document.body.innerHTML).not.toContain('s3cret');
  expect(fetcher.mock.calls.some(([u]) => String(u).endsWith('/inspection'))).toBe(false);
});

it('asks for an agent upgrade before editing when the host lacks container.configure', async () => {
  stub('organization_admin');
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText('Upgrade the host agent to enable editing.')).toBeTruthy();
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
});

it('shows the redacted configuration of a managed container with a link to its application', async () => {
  const fetcher = stub('organization_admin', [container()], { ...configurable, applications: [{ id: 'i1', application_id: 'app1', endpoint_id: 'ep_1', project: 'shop', containers: [{ id, name: 'web', image_id: 'sha256:1', created_at: '' }] }] });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect((await screen.findByRole('link', { name: 'Edit it there.' })).getAttribute('href')).toBe('/organizations/a/environments/env-a');
  expect(screen.getByText(/Managed by application shop/)).toBeTruthy();
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Save and recreate' })).toBeNull();
  expect(screen.queryByLabelText('Hostname')).toBeNull();
  expect(fetcher.mock.calls.some(([u]) => String(u).endsWith('/configuration'))).toBe(false);
});

it('stays on the page after a recreate settles and links to the new container', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const next = 'd'.repeat(64);
  stub('organization_admin', [container()], { ...configurable, command: { id: 'cmd9', action: 'container.recreate', outcome: 'succeeded', result: { steps: [{ service: 'direct', step: 'start', outcome: 'succeeded', detail: '' }], services: [{ service: 'direct', container_id: next, image_id: 'sha256:1', created_unix: 1 }] } } });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  fireEvent.change(await screen.findByLabelText('Hostname'), { target: { value: 'other' } });
  fireEvent.change(screen.getByLabelText(/^Type the container name/), { target: { value: 'web' } });
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save and recreate' })); });
  await act(async () => { vi.advanceTimersByTime(1500); });
  await act(async () => {});
  expect(window.location.pathname).toBe(`/organizations/a/endpoints/ep_1/containers/${id}`);
  expect(screen.getByText('Done.')).toBeTruthy();
  expect(within(screen.getByRole('table')).getByText('start')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Open the new container' }).getAttribute('href')).toBe(`/organizations/a/endpoints/ep_1/containers/${next}?tab=configuration`);
});

it('keeps the result and the link once the inventory drops the replaced container', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const next = 'd'.repeat(64);
  const containers = [container()];
  stub('organization_admin', containers, { ...configurable, command: { id: 'cmd9', action: 'container.recreate', outcome: 'succeeded', result: { steps: [{ service: 'direct', step: 'start', outcome: 'succeeded', detail: '' }], services: [{ service: 'direct', container_id: next, image_id: 'sha256:1', created_unix: 1 }] } } });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  fireEvent.change(await screen.findByLabelText('Hostname'), { target: { value: 'other' } });
  fireEvent.change(screen.getByLabelText(/^Type the container name/), { target: { value: 'web' } });
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save and recreate' })); });
  containers.length = 0;
  await act(async () => { vi.advanceTimersByTime(1500); });
  await act(async () => {});
  expect(await screen.findByText(/no longer reported/)).toBeTruthy();
  expect(within(screen.getByRole('table')).getByText('start')).toBeTruthy();
  expect(screen.getByText('Done.')).toBeTruthy();
  expect(screen.getAllByRole('link', { name: 'Open the new container' }).map((l) => l.getAttribute('href'))).toEqual([`/organizations/a/endpoints/ep_1/containers/${next}?tab=configuration`]);
});

it('keeps polling a sent recreate after the operator switches tabs', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  stub('organization_admin', [container()], { ...configurable, command: { id: 'cmd9', action: 'container.recreate', outcome: 'failed', result: { code: 'step_failed', steps: [{ service: 'direct', step: 'rollback', outcome: 'succeeded', code: 'start_failed_rolled_back', detail: '' }], services: [] } } });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  fireEvent.change(await screen.findByLabelText('Hostname'), { target: { value: 'other' } });
  fireEvent.change(screen.getByLabelText(/^Type the container name/), { target: { value: 'web' } });
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save and recreate' })); });
  expect(within(screen.getByRole('region', { name: 'Last change' })).getByText(/waiting for the host/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Activity' }));
  await act(async () => { vi.advanceTimersByTime(1500); });
  await act(async () => {});
  const panel = screen.getByRole('region', { name: 'Last change' });
  expect(within(panel).getByText('The host did not complete the change; the steps say which.')).toBeTruthy();
  expect(within(within(panel).getByRole('table')).getByText('rollback')).toBeTruthy();
});

it('says a run needs a Docker host, and shows only the load error when the host cannot be read', async () => {
  stub('organization_admin', [container()], { ep: { runtime: 'kubernetes', capabilities: ['container.configure', 'deployment.pull'] } });
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('Containers can be run only on a Docker host.')).toBeTruthy();
  cleanup();
  stub('organization_admin', [container()], { ...configurable, epStatus: 500 });
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  await act(async () => {});
  await act(async () => {});
  expect(screen.queryByText(/Upgrade the host agent|Docker host/)).toBeNull();
  expect(screen.queryByLabelText('Image reference')).toBeNull();
});

it('refuses the run form on a host whose agent cannot run containers', async () => {
  stub('organization_admin', [container()], { ep: { capabilities: ['container.configure'] } });
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('Upgrade the host agent to run containers here.')).toBeTruthy();
  expect(screen.queryByLabelText('Image reference')).toBeNull();
});

it('renders the run form for an administrator and refuses other roles', async () => {
  stub('organization_admin', [container()], configurable);
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  expect(((await screen.findByLabelText('Image reference')) as HTMLInputElement).value).toBe('');
  cleanup();
  stub('operator', [container()], configurable);
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('Only an organization administrator can run containers.')).toBeTruthy();
  expect(screen.queryByLabelText('Image reference')).toBeNull();
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
  expect(await screen.findByText(/Upgrade the host agent to enable live inspection/)).toBeTruthy();
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

it('says the organizations could not be read instead of refusing the role', async () => {
  stub('organization_admin', [container()], { ...configurable, orgStatus: 500 });
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('Something went wrong on the server. Retry, or check the server log.')).toBeTruthy();
  expect(screen.queryByText('Only an organization administrator can run containers.')).toBeNull();
});

it.each([['succeeded', true], ['denied', false]])('locks the run form after a %s run: %s', async (outcome, locked) => {
  stub('organization_admin', [container()], { ...configurable, run: { id: 'run1', action: 'container.run', outcome, result: { steps: [], services: [] } } });
  render(<ContainerRunPage org="a" endpoint="ep_1" />);
  fireEvent.change(await screen.findByLabelText('Image reference'), { target: { value: 'nginx:1' } });
  fireEvent.change(screen.getByLabelText('Container name'), { target: { value: 'api' } });
  const typeName = () => fireEvent.change(screen.getByLabelText(/^Type the new container name/), { target: { value: 'api' } });
  typeName();
  const run = screen.getByRole('button', { name: 'Run container' }) as HTMLButtonElement;
  expect(run.disabled).toBe(false);
  await act(async () => { fireEvent.click(run); });
  expect(await screen.findByText(outcome === 'succeeded' ? 'Done.' : 'The host refused the change; nothing was replaced.')).toBeTruthy();
  expect(screen.getByLabelText('Image reference').matches(':disabled')).toBe(locked);
  typeName();
  expect(run.disabled).toBe(locked);
});

it('refreshes the host details with the inventory every 30 s', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const fetcher = stub();
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  await act(async () => {});
  const detailCalls = () => fetcher.mock.calls.filter(([u]) => String(u) === '/api/organizations/a/endpoints/ep_1').length;
  const before = detailCalls();
  expect(before).toBeGreaterThan(0);
  await act(async () => { vi.advanceTimersByTime(30_000); });
  await act(async () => {});
  expect(detailCalls()).toBe(before + 1);
});

it.each([
  [401, 'Your session has expired. Sign in again.'],
  [403, 'You do not have permission to edit this container.'],
  [429, 'Too many configuration requests. Wait a minute and try again.'],
  [501, 'Upgrade the host agent to enable editing.'],
  [504, 'The host did not answer in time. Try again.'],
])('maps a %i configuration read to a fixed text with Read again', async (status, text) => {
  stub('organization_admin', [container()], { ...configurable, configurationStatus: status });
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Read again' })).toBeTruthy();
});
