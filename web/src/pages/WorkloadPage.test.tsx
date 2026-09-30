import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { WorkloadPage } from './WorkloadPage';
import { App } from '../App';
vi.mock('../components/PodTerminal', () => ({ PodTerminal: ({ pod }: { pod: { name: string } }) => <p>pod terminal stub {pod.name}</p> }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, '', '/'); document.cookie = 'ky_csrf=; Max-Age=0'; });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const base = '/api/organizations/a/endpoints/ep_k';
const path = '/organizations/a/endpoints/ep_k/workloads/shop/deployment/web';
const SECRET = 'page-literal-value';
const endpoint = { id: 'ep_k', environment_id: 'env-a', name: 'prod', runtime: 'kubernetes', state: 'active', facts: {}, fingerprint: '', capabilities: ['kubernetes.inventory', 'pod.logs', 'kubernetes.workloads', 'pod.exec'], alerts: [], created_at: '', deploy_namespaces: ['shop'] };
const web = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 3, ready: 2, updated: 1, images: ['nginx:1.29'], paused: true };
const podContainer = (name: string, restarts: number) => ({ name, image: 'nginx:1.29', image_id: '', state: 'running', reason: '', ready: true, restart_count: restarts });
const pods = [
  { uid: 'u-1', namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'worker-1', owner_kind: 'Deployment', owner_name: 'web', started_at: '2026-09-29T09:00:00Z', containers: [podContainer('web', 2), podContainer('log', 1)] },
  { uid: 'u-2', namespace: 'shop', name: 'web-8d1', phase: 'Pending', node: '', owner_kind: 'Deployment', owner_name: 'web', started_at: '0001-01-01T00:00:00Z', containers: [] },
  { uid: 'u-3', namespace: 'shop', name: 'other-1', phase: 'Running', node: 'worker-1', owner_kind: 'Deployment', owner_name: 'other', started_at: '', containers: [] },
];
const configuration = {
  target: { namespace: 'shop', kind: 'deployment', name: 'web' }, observed_at: new Date().toISOString(), resource_version: '41', replicas: 3, paused: true, strategy: 'RollingUpdate',
  containers: [{ name: 'web', image: 'nginx:1.29', image_id: '', command: [], args: [], env: [{ name: 'MODE', value: SECRET }], resources: { cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' } }],
  init_containers: [], env_from: [], managed: false, unsupported: [],
};
type Opts = { workload?: object; applications?: object[]; command?: object; ep?: object; configurationStatus?: number };
function stub(role = 'organization_admin', opts: Opts = {}) {
  const now = new Date().toISOString();
  const requests: { url: string; init?: RequestInit }[] = [];
  const kubernetes = { nodes: [], namespaces: ['shop'], workloads: [{ ...web, ...opts.workload }], pods, services: [], claims: [] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    requests.push({ url, init });
    if (url === '/api/auth/me') return json({ authenticated: true, user: { id: 'u', username: 'admin', role: 'user' } });
    if (url === '/api/settings') return json({ app_name: 'KyYard' });
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Team', role }]);
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_k', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'kubernetes', version: 'v1.36', api_version: '', os: '', arch: '', kernel: '', cpus: 0, memory_bytes: 0, hostname: '' }, containers: [], images: [], networks: [], volumes: [], kubernetes } });
    if (url.endsWith('/applications')) return json(opts.applications ?? []);
    if (url.endsWith('/configuration')) return opts.configurationStatus ? json({ code: 'application_managed' }, opts.configurationStatus) : json(configuration);
    if (url.endsWith('/apply')) return json({ id: 'k7', action: 'workload.apply', reference: 'shop/deployment/web', outcome: '' }, 202);
    if (url.endsWith('/commands/k7')) return json(opts.command ?? { id: 'k7', action: 'workload.apply', outcome: '' });
    if (url.includes('/commands')) return json([{ id: 'k1', action: 'workload.scale', reference: 'shop/deployment/web', outcome: 'denied', detail: 'forbidden', created_at: now }]);
    if (url.includes('/pods/')) return new Response('log line\n', { status: 200 });
    return json({ ...endpoint, ...opts.ep });
  }));
  return requests;
}
const page = () => render(<WorkloadPage org="a" endpoint="ep_k" namespace="shop" kind="deployment" workload="web" />);
const tab = (name: string) => fireEvent.click(within(screen.getByRole('navigation', { name: 'Workload sections' })).getByRole('button', { name }));

it('dispatches the workload route from the App', async () => {
  stub();
  window.history.replaceState(null, '', path);
  render(<App />);
  expect(await screen.findByRole('heading', { level: 1, name: /shop\/web/ })).toBeTruthy();
});

it('shows the overview with replicas, images and only the workload\'s pods', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  stub();
  page();
  const overview = await screen.findByRole('region', { name: 'Overview' });
  expect(overview.textContent).toContain('Deployment');
  expect(overview.textContent).toContain('3 desired · 2 ready · 1 updated');
  expect(overview.textContent).toContain('Paused');
  expect(overview.textContent).toContain('nginx:1.29');
  expect(screen.getByText('shop/web-7c9')).toBeTruthy();
  expect(screen.getByText('shop/web-8d1')).toBeTruthy();
  expect(screen.queryByText('shop/other-1')).toBeNull();
  const row = screen.getByText('shop/web-7c9').closest('tr')!;
  expect(row.textContent).toContain('1h 0m');
  expect(row.textContent).toContain('worker-1');
  expect(row.textContent).toContain('3');
  expect(within(row).getByRole('button', { name: 'Delete shop/web-7c9' })).toBeTruthy();
  expect(screen.getByRole('group', { name: 'Actions for shop/web' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'prod' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k');
});

it('says when the workload left the inventory and renders no controls', async () => {
  stub('organization_admin', { workload: { name: 'gone' } });
  page();
  expect(await screen.findByText(/no longer reported/)).toBeTruthy();
  expect(screen.queryByRole('group', { name: 'Actions for shop/web' })).toBeNull();
});

it.each([['organization_admin', true], ['operator', false]])('offers the Terminal tab to %s: %s', async (role, want) => {
  stub(role);
  page();
  await screen.findByRole('region', { name: 'Overview' });
  await act(async () => {});
  expect(within(screen.getByRole('navigation', { name: 'Workload sections' })).queryByRole('button', { name: 'Terminal' }) !== null).toBe(want);
});

it('shows a managed workload read-only with a link to its application and never reads it', async () => {
  const requests = stub('organization_admin', { workload: { application: 'app', instance: 'i1' }, applications: [{ id: 'i1', application_id: 'app', project: 'storefront', endpoint_id: 'ep_k', containers: [] }] });
  window.history.replaceState(null, '', `${path}?tab=configuration`);
  page();
  const note = await screen.findByText(/Managed by application storefront\./);
  expect(within(note).getByRole('link', { name: 'Edit it there.' }).getAttribute('href')).toBe('/organizations/a/environments/env-a');
  expect(screen.queryByRole('button', { name: 'Save and apply' })).toBeNull();
  expect(requests.some((r) => r.url.endsWith('/configuration'))).toBe(false);
});

it('treats a 409 application_managed read as managed and links to the cluster when the application is unknown', async () => {
  stub('organization_admin', { configurationStatus: 409 });
  window.history.replaceState(null, '', `${path}?tab=configuration`);
  page();
  const note = await screen.findByText(/Managed by a KyYard application\./);
  expect(within(note).getByRole('link', { name: 'Edit it there.' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k');
  expect(screen.queryByRole('button', { name: 'Save and apply' })).toBeNull();
});

it('shows a summary, not the form, to a role that cannot configure', async () => {
  const requests = stub('operator');
  window.history.replaceState(null, '', `${path}?tab=configuration`);
  page();
  expect(await screen.findByText(/Only an organization administrator can edit/)).toBeTruthy();
  expect(screen.getByRole('region', { name: 'Configuration' }).textContent).toContain('nginx:1.29');
  expect(requests.some((r) => r.url.endsWith('/configuration'))).toBe(false);
});

it('applies an edit and keeps the step table in Last change across a tab switch', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const requests = stub('organization_admin', { command: { id: 'k7', action: 'workload.apply', outcome: 'denied', result: { code: 'step_failed', steps: [{ service: 'workload', step: 'precondition', outcome: 'denied', code: 'conflict', detail: '' }], services: [] } } });
  document.cookie = 'ky_csrf=csrf-p';
  window.history.replaceState(null, '', `${path}?tab=configuration`);
  const { container } = page();
  await screen.findByLabelText('Image of web');
  expect(container.innerHTML).not.toContain(SECRET);
  expect(requests.find((r) => r.url.endsWith('/configuration'))?.url).toBe(`${base}/workloads/shop/deployment/web/configuration`);
  fireEvent.change(screen.getByLabelText('Image of web'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save and apply' }));
  const last = await screen.findByRole('region', { name: 'Last change' });
  expect(last.textContent).toContain('waiting for the cluster agent');
  const conflict = 'The workload changed since you read it. Read again.';
  for (let i = 0; i < 20 && !screen.queryByText(conflict); i++) await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
  expect(within(last).getByText(conflict)).toBeTruthy();
  // A settled apply re-reads, so resource_version and the diff are fresh; after a conflict the
  // ready form also offers Read again.
  for (let i = 0; i < 20 && requests.filter((r) => r.url.endsWith('/configuration')).length < 2; i++) await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  await act(async () => { await vi.advanceTimersByTimeAsync(0); });
  expect(requests.filter((r) => r.url.endsWith('/configuration'))).toHaveLength(2);
  expect(screen.getByLabelText('Image of web')).toHaveProperty('value', 'nginx:1.29');
  fireEvent.click(screen.getByRole('button', { name: 'Read again' }));
  for (let i = 0; i < 20 && requests.filter((r) => r.url.endsWith('/configuration')).length < 3; i++) await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  expect(requests.filter((r) => r.url.endsWith('/configuration'))).toHaveLength(3);
  tab('Overview');
  expect(within(screen.getByRole('region', { name: 'Last change' })).getByText('The workload changed since you read it. Read again.')).toBeTruthy();
});

it('does not re-read the configuration when a second apply is sent until it settles', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const opts: Opts = { command: { id: 'k7', action: 'workload.apply', outcome: 'succeeded' } };
  const requests = stub('organization_admin', opts);
  document.cookie = 'ky_csrf=csrf-p';
  window.history.replaceState(null, '', `${path}?tab=configuration`);
  page();
  await screen.findByLabelText('Image of web');
  const edit = async (image: string) => {
    fireEvent.change(await screen.findByLabelText('Image of web'), { target: { value: image } });
    fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and apply' }));
  };
  const reads = () => requests.filter((r) => r.url.endsWith('/configuration')).length;
  await edit('nginx:1.30');
  await screen.findByRole('region', { name: 'Last change' });
  await act(async () => { vi.advanceTimersByTime(1500); });
  for (let i = 0; i < 4; i++) await act(async () => {});
  expect(reads()).toBe(2);
  opts.command = { id: 'k7', action: 'workload.apply', outcome: '' };
  await edit('nginx:1.31');
  for (let i = 0; i < 4; i++) await act(async () => {});
  expect(reads()).toBe(2);
});

it('reads logs for the chosen pod and container inline', async () => {
  const requests = stub();
  window.history.replaceState(null, '', `${path}?tab=logs&pod=web-7c9`);
  page();
  fireEvent.change(await screen.findByLabelText('Container'), { target: { value: 'log' } });
  fireEvent.click(screen.getByRole('button', { name: 'Load logs' }));
  expect(await screen.findByText('log line')).toBeTruthy();
  expect(requests.some((r) => r.url.startsWith(`${base}/pods/shop/web-7c9/logs?container=log&tail=200`))).toBe(true);
});

it('opens the pod terminal only on a running pod', async () => {
  stub();
  window.history.replaceState(null, '', `${path}?tab=terminal&pod=web-8d1`);
  page();
  expect(await screen.findByText(/needs a running pod/)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Pod'), { target: { value: 'web-7c9' } });
  expect(await screen.findByText('pod terminal stub web-7c9')).toBeTruthy();
});

it('says to upgrade when the agent cannot open pod terminals', async () => {
  stub('organization_admin', { ep: { capabilities: ['kubernetes.inventory', 'kubernetes.workloads'] } });
  window.history.replaceState(null, '', `${path}?tab=terminal`);
  page();
  expect(await screen.findByText('Upgrade the cluster agent and re-apply the manifest to enable this.')).toBeTruthy();
});

it('lists activity for the workload reference with fixed detail texts', async () => {
  const requests = stub();
  window.history.replaceState(null, '', `${path}?tab=activity`);
  page();
  expect(await screen.findByText(/Scale refused\. The agent's role does not allow this/)).toBeTruthy();
  expect(screen.queryByText(/workload\.scale/)).toBeNull();
  expect(screen.getByText(/Pod commands appear under the cluster's Activity tab/)).toBeTruthy();
  expect(requests.some((r) => r.url === `${base}/commands?reference=shop%2Fdeployment%2Fweb&limit=50`)).toBe(true);
});

it('hides workload controls when the agent lacks kubernetes.workloads', async () => {
  stub('organization_admin', { ep: { capabilities: ['kubernetes.inventory'] } });
  page();
  await screen.findByRole('region', { name: 'Overview' });
  await waitFor(() => expect(screen.getByRole('link', { name: 'Logs for shop/web-7c9' })).toBeTruthy());
  await act(async () => {});
  expect(screen.queryByRole('button', { name: /^(Restart|Scale|Delete) / })).toBeNull();
});
