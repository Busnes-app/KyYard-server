import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { WorkloadControls } from './WorkloadControls';
import { PodControls } from './PodControls';
import type { Pod, Workload } from '../tenant';
import { commandLine, WORKLOAD_STEPS } from './workloadTexts';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const base = '/api/organizations/a/endpoints/ep_k';
const caps = ['kubernetes.inventory', 'kubernetes.workloads', 'pod.exec'];
const web: Workload = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 3, ready: 3, updated: 3, images: ['nginx:1'], paused: false };
const logs: Workload = { ...web, kind: 'DaemonSet', name: 'logs' };
const pod: Pod = { uid: 'u-1', namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'n1', owner_kind: 'Deployment', owner_name: 'web', started_at: '', containers: [] };

function posts(reply: (body: Record<string, unknown>) => Response = () => json({ id: 'k1', action: 'workload.restart', outcome: '' }, 202)) {
  const sent: Record<string, unknown>[] = [];
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') {
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-w');
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      sent.push(body);
      return reply(body);
    }
    return json({ id: 'k1', action: 'workload.restart', outcome: '' });
  });
  vi.stubGlobal('fetch', fetcher);
  document.cookie = 'ky_csrf=csrf-w';
  return sent;
}
const controls = (over: Partial<Parameters<typeof WorkloadControls>[0]> = {}) => {
  const onStatus = vi.fn();
  render(<WorkloadControls base={base} org="a" endpoint="ep_k" workload={web} active role="organization_admin" capabilities={caps} scope="Cluster prod" onStatus={onStatus} open {...over} />);
  return onStatus;
};

it('restarts, scales and deletes a Deployment with the exact command bodies', async () => {
  const sent = posts();
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const prompt = vi.spyOn(window, 'prompt');
  const onStatus = controls();
  fireEvent.click(screen.getByRole('button', { name: 'Restart shop/web' }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({ action: 'workload.restart', reference: 'shop/deployment/web' });
  await waitFor(() => expect(onStatus).toHaveBeenLastCalledWith('shop/web · Command sent; waiting for the cluster agent. Do not retry while its outcome is unknown.'));
  cleanup();
  const sent2 = posts();
  controls();
  prompt.mockReturnValueOnce('5');
  fireEvent.click(screen.getByRole('button', { name: 'Scale shop/web' }));
  await waitFor(() => expect(sent2).toHaveLength(1));
  expect(sent2[0]).toEqual({ action: 'workload.scale', reference: 'shop/deployment/web', expects: { replicas: 5 } });
  cleanup();
  const sent3 = posts();
  controls();
  prompt.mockReturnValueOnce('web');
  fireEvent.click(screen.getByRole('button', { name: 'Delete shop/web' }));
  await waitFor(() => expect(sent3).toHaveLength(1));
  expect(sent3[0]).toEqual({ action: 'workload.delete', reference: 'shop/deployment/web', confirm: 'web' });
  expect(screen.getByRole('link', { name: 'Open shop/web' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k/workloads/shop/deployment/web');
});

it('sends nothing for an out-of-range scale or a mistyped delete name', async () => {
  const sent = posts();
  const prompt = vi.spyOn(window, 'prompt');
  const onStatus = controls();
  prompt.mockReturnValueOnce('1001');
  fireEvent.click(screen.getByRole('button', { name: 'Scale shop/web' }));
  expect(onStatus).toHaveBeenLastCalledWith('shop/web · Enter a whole number of replicas from 0 to 1000.');
  prompt.mockReturnValueOnce('2.5');
  fireEvent.click(screen.getByRole('button', { name: 'Scale shop/web' }));
  prompt.mockReturnValueOnce('we');
  fireEvent.click(screen.getByRole('button', { name: 'Delete shop/web' }));
  await act(async () => {});
  expect(sent).toHaveLength(0);
});

it('offers no Scale on a DaemonSet', () => {
  posts();
  controls({ workload: logs });
  expect(screen.getByRole('button', { name: 'Restart shop/logs' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Scale shop/logs' })).toBeNull();
});

it.each([
  ['organization_admin', caps, ['Restart', 'Scale', 'Delete']],
  ['environment_admin', caps, ['Restart', 'Scale', 'Delete']],
  ['operator', caps, ['Restart', 'Scale']],
  ['developer', caps, []],
  ['read_only', caps, []],
  ['organization_admin', ['kubernetes.inventory'], []],
])('%s with %j gets %j', (role, capabilities, want) => {
  posts();
  controls({ role, capabilities });
  const got = ['Restart', 'Scale', 'Delete'].filter((a) => screen.queryByRole('button', { name: `${a} shop/web` }) !== null);
  expect(got).toEqual(want);
});

it('reports a settled command with fixed text and never the raw detail', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const sent = posts();
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST'
    ? (sent.push({}), json({ id: 'k1', action: 'workload.restart', outcome: '' }, 202))
    : json({ id: 'k1', action: 'workload.restart', outcome: 'denied', detail: 'forbidden' })));
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const onStatus = controls();
  fireEvent.click(screen.getByRole('button', { name: 'Restart shop/web' }));
  await act(async () => {});
  await act(async () => { vi.advanceTimersByTime(1500); });
  await act(async () => {});
  expect(onStatus).toHaveBeenLastCalledWith("shop/web · Restart refused. The agent's role does not allow this. Regenerate and apply the cluster manifest, then retry.");
});

it.each([
  [json({ code: 'application_managed' }, 409), 'shop/web · Managed by a KyYard application. Edit it there.'],
  [json({ code: 'namespace_not_granted' }, 422), 'shop/web · This namespace is not granted to KyYard on this cluster.'],
  [json({ error: 'Upgrade the cluster agent to operate workloads' }, 501), 'shop/web · Upgrade the cluster agent and re-apply the manifest to enable this.'],
  [json({}, 404), 'shop/web · The workload is no longer in the cluster.'],
])('maps a refused delete to a fixed text', async (response, text) => {
  posts(() => response);
  vi.spyOn(window, 'prompt').mockReturnValue('web');
  const onStatus = controls();
  fireEvent.click(screen.getByRole('button', { name: 'Delete shop/web' }));
  await waitFor(() => expect(onStatus).toHaveBeenLastCalledWith(text));
});

it('deletes a pod by its typed name and links logs and terminal to its workload page', async () => {
  const sent = posts();
  vi.spyOn(window, 'prompt').mockReturnValue('web-7c9');
  const onStatus = vi.fn();
  render(<PodControls base={base} org="a" endpoint="ep_k" pod={pod} active role="organization_admin" capabilities={caps} scope="Cluster prod" onStatus={onStatus} />);
  fireEvent.click(screen.getByRole('button', { name: 'Delete shop/web-7c9' }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toEqual({ action: 'pod.delete', reference: 'shop/pod/web-7c9', confirm: 'web-7c9' });
  expect(screen.getByRole('link', { name: 'Logs for shop/web-7c9' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k/workloads/shop/deployment/web?tab=logs&pod=web-7c9');
  expect(screen.getByRole('link', { name: 'Terminal for shop/web-7c9' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k/workloads/shop/deployment/web?tab=terminal&pod=web-7c9');
});

it.each([
  ['operator', caps, false, false],
  ['environment_admin', caps, true, false],
  ['organization_admin', ['kubernetes.inventory', 'kubernetes.workloads'], true, false],
  ['organization_admin', ['kubernetes.inventory', 'pod.exec'], false, true],
])('gates pod controls for %s with %j', (role, capabilities, remove, terminal) => {
  posts();
  render(<PodControls base={base} org="a" endpoint="ep_k" pod={pod} active role={role} capabilities={capabilities} scope="Cluster prod" onStatus={vi.fn()} />);
  expect(screen.queryByRole('button', { name: 'Delete shop/web-7c9' }) !== null).toBe(remove);
  expect(screen.queryByRole('link', { name: 'Terminal for shop/web-7c9' }) !== null).toBe(terminal);
});

it('offers no page links for a pod without a configurable owner', () => {
  posts();
  render(<PodControls base={base} org="a" endpoint="ep_k" pod={{ ...pod, owner_kind: 'Job', owner_name: 'migrate' }} active role="organization_admin" capabilities={caps} scope="Cluster prod" onStatus={vi.fn()} />);
  expect(screen.queryByRole('link', { name: /for shop\/web-7c9/ })).toBeNull();
  expect(screen.getByRole('button', { name: 'Delete shop/web-7c9' })).toBeTruthy();
});

it('points a lost connection at the cluster Activity tab', async () => {
  posts(() => { throw new TypeError('Failed to fetch'); });
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const onStatus = controls();
  fireEvent.click(screen.getByRole('button', { name: 'Restart shop/web' }));
  await waitFor(() => expect(onStatus).toHaveBeenLastCalledWith("shop/web · Connection lost. The action may have run. Check the cluster's Activity tab before trying again."));
});

it('hides Delete for an application-managed workload', () => {
  posts();
  controls({ workload: { ...web, application: 'app-1' } });
  expect(screen.queryByRole('button', { name: 'Delete shop/web' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Restart shop/web' })).toBeTruthy();
});

it('renders no controls for a name the route rejects', () => {
  posts();
  controls({ workload: { ...web, name: 'web.v2' } });
  expect(screen.queryByRole('group')).toBeNull();
});

it('words a managed delete the agent settled as denied and a Pod Security refusal', () => {
  expect(commandLine({ action: 'workload.delete', outcome: 'denied', detail: 'application_managed' })).toBe('Delete refused. Managed by a KyYard application. Edit it there.');
  expect(commandLine({ action: 'workload.apply', outcome: 'denied', detail: 'pod_security' })).toBe('Apply refused. This namespace does not enforce Pod Security baseline or restricted; KyYard refuses to change workloads there.');
});

it('words a scale-down refused to keep a StatefulSet\'s volume claims', () => {
  expect(commandLine({ action: 'workload.scale', outcome: 'denied', detail: 'pvc_retention' })).toBe("Scale refused. Scaling down would delete this StatefulSet's volume claims (whenScaled: Delete).");
  expect(WORKLOAD_STEPS.pvc_retention).toBe("Scaling down would delete this StatefulSet's volume claims (whenScaled: Delete).");
});
