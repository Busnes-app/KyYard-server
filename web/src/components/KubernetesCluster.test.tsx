import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { KubernetesCluster } from './KubernetesCluster';
import type { Endpoint, KubernetesInventory } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const endpoint = { id: 'ep_k', environment_id: 'env-a', name: 'prod', runtime: 'kubernetes', state: 'active', facts: {}, fingerprint: '', capabilities: [], alerts: [], created_at: '', deploy_namespaces: [] } as Endpoint;
const empty: KubernetesInventory = { nodes: [], namespaces: [], workloads: [], pods: [], services: [], claims: [] };
const view = (inventory: KubernetesInventory, e: Endpoint = endpoint) => <KubernetesCluster org="a" base="/api/organizations/a/endpoints/ep_k" endpoint={e} inventory={inventory} instances={[]} admin={false} onChanged={vi.fn()} />;

it('shows a cluster that reports no nodes as unknown, not healthy', () => {
  render(view(empty));
  const health = screen.getByRole('region', { name: 'Cluster health' });
  expect(health.textContent).toContain('unknown');
  expect(health.textContent).toContain('0 of 0 nodes ready.');
  expect(screen.getByText('No nodes reported.')).toBeTruthy();
});

it('lists a pod with no containers reported yet, with nothing to open', () => {
  render(view({ ...empty, namespaces: ['shop'], pods: [{ namespace: 'shop', name: 'web-1', phase: 'Pending', node: '', owner_kind: '', owner_name: '', started_at: '', containers: [] }] }));
  expect(screen.getByText('shop/web-1')).toBeTruthy();
  expect(screen.getByText('Pending')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Logs for/ })).toBeNull();
});

it('drops a namespace filter whose namespace left the cluster', () => {
  const pod = (namespace: string, name: string) => ({ namespace, name, phase: 'Running', node: 'n1', owner_kind: '', owner_name: '', started_at: '', containers: [] });
  const both = { ...empty, namespaces: ['billing', 'shop'], pods: [pod('billing', 'pay-1'), pod('shop', 'web-1')] };
  const { rerender } = render(view(both));
  fireEvent.change(screen.getByRole('combobox', { name: 'Namespace' }), { target: { value: 'shop' } });
  expect(screen.queryByText('billing/pay-1')).toBeNull();
  rerender(view({ ...both, namespaces: ['billing'], pods: [pod('billing', 'pay-1')] }));
  expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveProperty('value', '');
  expect(screen.getByText('billing/pay-1')).toBeTruthy();
});

it('shows pod uptime from started_at and a dash for the zero time', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  const inventory: KubernetesInventory = { ...empty, namespaces: ['shop'], pods: [
    { namespace: 'shop', name: 'web-1', phase: 'Running', node: 'n1', owner_kind: 'ReplicaSet', owner_name: 'web', started_at: '2026-09-29T09:00:00Z', containers: [] },
    { namespace: 'shop', name: 'web-2', phase: 'Pending', node: '', owner_kind: '', owner_name: '', started_at: '0001-01-01T00:00:00Z', containers: [] },
  ] };
  render(<KubernetesCluster org="a" base="/api/x" endpoint={endpoint} inventory={inventory} instances={[]} admin={false} onChanged={() => {}} />);
  const rows = screen.getAllByRole('row').slice(1);
  expect(rows[0].textContent).toContain('1h 0m');
  expect(rows[1].querySelectorAll('td')[2].textContent).toBe('—');
  vi.useRealTimers();
});

it('links configurable workloads to their page and renders toolbars by role and capability', () => {
  const inventory: KubernetesInventory = { ...empty, namespaces: ['shop'],
    workloads: [{ kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: [], paused: false }, { kind: 'CronJob', namespace: 'shop', name: 'nightly', desired: 0, ready: 0, updated: 0, images: [], paused: false }],
    pods: [{ uid: 'u', namespace: 'shop', name: 'web-1', phase: 'Running', node: 'n1', owner_kind: 'Deployment', owner_name: 'web', started_at: '', containers: [] }] };
  const capable = { ...endpoint, capabilities: ['kubernetes.inventory', 'kubernetes.workloads'] };
  const cluster = (e: Endpoint, role: string) => <KubernetesCluster org="a" base="/api/x" endpoint={e} inventory={inventory} instances={[]} admin={false} role={role} onStatus={vi.fn()} onChanged={vi.fn()} />;
  const { rerender } = render(cluster(capable, 'operator'));
  expect(screen.getByRole('link', { name: 'shop/web' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_k/workloads/shop/deployment/web');
  expect(screen.queryByRole('link', { name: 'shop/nightly' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Restart shop/web' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Delete shop/web-1' })).toBeNull();
  expect(screen.queryByRole('group', { name: 'Actions for shop/nightly' })).toBeNull();
  rerender(cluster(capable, 'environment_admin'));
  expect(screen.getByRole('button', { name: 'Delete shop/web-1' })).toBeTruthy();
  rerender(cluster(endpoint, 'organization_admin'));
  expect(screen.queryByRole('button', { name: 'Restart shop/web' })).toBeNull();
  expect(screen.queryByText('Actions')).toBeNull();
});

it('gives an ownerless pod Delete and a Logs button opening the logs dialog', () => {
  const show = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: show });
  const c = { name: 'task', image: 'busybox', image_id: '', state: 'running', reason: '', ready: true, restart_count: 0 };
  const inventory: KubernetesInventory = { ...empty, namespaces: ['shop'], pods: [{ uid: 'u', namespace: 'shop', name: 'job-1', phase: 'Running', node: 'n1', owner_kind: 'Job', owner_name: 'migrate', started_at: '', containers: [c] }] };
  const capable = { ...endpoint, capabilities: ['kubernetes.inventory', 'kubernetes.workloads'] };
  render(<KubernetesCluster org="a" base="/api/x" endpoint={capable} inventory={inventory} instances={[]} admin={false} role="organization_admin" onStatus={vi.fn()} onChanged={vi.fn()} />);
  const group = screen.getByRole('group', { name: 'Actions for shop/job-1' });
  expect(within(group).getByRole('button', { name: 'Delete shop/job-1' })).toBeTruthy();
  fireEvent.click(within(group).getByRole('button', { name: 'Logs for shop/job-1' }));
  expect(screen.getByRole('dialog', { name: 'Logs for shop/job-1/task' })).toBeTruthy();
  expect(show).toHaveBeenCalledOnce();
  Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal');
});

it('reloads the inventory when a cluster toolbar command settles', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => new Response(JSON.stringify(
    init?.method === 'POST' ? { id: 'k1', action: 'workload.restart', outcome: '' } : { id: 'k1', action: 'workload.restart', outcome: 'succeeded' }),
    { status: init?.method === 'POST' ? 202 : 200, headers: { 'Content-Type': 'application/json' } })));
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
  const onRefresh = vi.fn();
  const inventory: KubernetesInventory = { ...empty, namespaces: ['shop'], workloads: [{ kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: [], paused: false }] };
  const capable = { ...endpoint, capabilities: ['kubernetes.inventory', 'kubernetes.workloads'] };
  render(<KubernetesCluster org="a" base="/api/x" endpoint={capable} inventory={inventory} instances={[]} admin={false} role="operator" onStatus={vi.fn()} onRefresh={onRefresh} onChanged={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Restart shop/web' }));
  await act(async () => {});
  await act(async () => { vi.advanceTimersByTime(1500); });
  await act(async () => {});
  expect(onRefresh).toHaveBeenCalledOnce();
  confirm.mockRestore();
  vi.useRealTimers();
});
