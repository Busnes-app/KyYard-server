import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { KubernetesMapping, KubernetesWorkloads, KUBERNETES_UNVALIDATED } from './KubernetesMapping';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const cluster = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['billing', 'shop'] };
const host = { id: 'ep_d', name: 'docker', runtime: 'docker', state: 'active' };

it('maps to a namespace the cluster grants, and only to a cluster', async () => {
  document.cookie = 'ky_csrf=csrf';
  const changed = vi.fn();
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? new Response(null, { status: 204 }) : json([host, cluster]));
  vi.stubGlobal('fetch', fetcher);
  render(<KubernetesMapping base="/app" org="a" env="e" admin={false} onChanged={changed} />);
  const select = await screen.findByLabelText('Cluster');
  expect([...select.querySelectorAll('option')].map((o) => o.textContent)).toEqual(['Choose a cluster', 'prod · active']);
  fireEvent.change(select, { target: { value: 'ep_k' } });
  const save = screen.getByRole('button', { name: 'Save namespace mapping' });
  expect(save.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Namespace'), { target: { value: 'shop' } });
  fireEvent.click(save);
  await vi.waitFor(() => expect(changed).toHaveBeenCalledWith('Mapped to namespace shop. Nothing was deployed.'));
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(String(put?.[0])).toBe('/app/mapping');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ endpoint_id: 'ep_k', namespace: 'shop' });
  expect(new Headers(put?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
  expect(screen.queryByRole('button', { name: 'Regenerate manifest' })).toBeNull();
});

it('explains a namespace the manifest does not grant, and offers administrators the manifest', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? json({ code: 'namespace_unknown', error: 'secret-canary' }, 400) : json([cluster])));
  render(<KubernetesMapping base="/app" org="a" env="e" admin onChanged={vi.fn()} />);
  fireEvent.change(await screen.findByLabelText('Cluster'), { target: { value: 'ep_k' } });
  fireEvent.change(screen.getByLabelText('Namespace'), { target: { value: 'billing' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save namespace mapping' }));
  expect((await screen.findByRole('alert')).textContent).toContain("The cluster's manifest does not grant that namespace.");
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.getByRole('button', { name: 'Regenerate manifest' })).toBeTruthy();
});

it('renders nothing without a cluster', async () => {
  const fetcher = vi.fn(async () => json([host]));
  vi.stubGlobal('fetch', fetcher);
  const { container } = render(<KubernetesMapping base="/app" org="a" env="e" admin onChanged={vi.fn()} />);
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalled());
  await vi.waitFor(() => expect(container.textContent).toBe(''));
});

it("lists the instance's Deployments from the cluster inventory and says health is not validated", async () => {
  const instance = { id: 'i1', application_id: 'app', endpoint_id: 'ep_k', endpoint_name: 'prod', project: 'shop', namespace: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 1, container_count: 0, containers: [] };
  const workloads = [
    { kind: 'Deployment', namespace: 'shop', name: 'shop-web', desired: 1, ready: 1, updated: 1, images: ['ghcr.io/org/web@sha256:abc'], paused: false, instance: 'i1' },
    { kind: 'Deployment', namespace: 'shop', name: 'foreign', desired: 1, ready: 0, updated: 1, images: ['x'], paused: false },
  ];
  vi.stubGlobal('fetch', vi.fn(async () => json({ snapshot: { kubernetes: { workloads } } })));
  render(<KubernetesWorkloads org="a" instance={instance} />);
  expect(await screen.findByText('shop-web')).toBeTruthy();
  expect(screen.getByText('1/1')).toBeTruthy();
  expect(screen.queryByText('foreign')).toBeNull();
  expect(screen.getByText(KUBERNETES_UNVALIDATED)).toBeTruthy();
});
