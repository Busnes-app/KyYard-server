import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { KubernetesNetworking } from './KubernetesNetworking';

const instance = { id: 'i1', application_id: 'app', endpoint_id: 'ep_k', endpoint_name: 'prod', project: 'shop', namespace: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 1, container_count: 0, containers: [] };
const revision = { digest: 'digest', spec: { services: [{ name: 'web', image: 'nginx:1', ports: [{ target: 80, published: 8080, protocol: 'tcp' }] }, { name: 'worker', image: 'worker:1' }], kubernetes: { service_ips: { web: '10.96.0.40' } } } };
const inventory = { received_at: '2026-09-27T12:00:00Z', snapshot: { kubernetes: { services: [
  { name: 'shop-web', namespace: 'shop', service: 'web', instance: 'i1', cluster_ip: 'fd00::40', ports: ['8080/TCP'] },
  { name: 'other-web', namespace: 'other', service: 'web', instance: 'i2', cluster_ip: '10.96.0.99', ports: ['8080/TCP'] },
] } } };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

it('loads on demand, distinguishes desired and observed addresses, and saves only networking with CSRF', async () => {
  document.cookie = 'ky_csrf=csrf';
  const saved = vi.fn();
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? json({ revision: 2 }, 201) : json(String(url).endsWith('/inventory') ? inventory : revision));
  vi.stubGlobal('fetch', fetcher);
  render(<KubernetesNetworking base="/app" org="a" instance={instance} expected={1} editable onSaved={saved} />);
  expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  const input = await screen.findByLabelText('Internal IP for web');
  expect(input).toHaveProperty('value', '10.96.0.40');
  expect(await screen.findByText('http://[fd00::40]:8080')).toBeTruthy();
  expect(document.body.textContent).not.toContain('10.96.0.99');
  expect(screen.queryByLabelText('Internal IP for worker')).toBeNull();
  fireEvent.change(input, { target: { value: ' 10.96.0.41 ' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save networking as revision 2' }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  const put = fetcher.mock.calls.find(([, init]) => init?.method === 'PUT');
  expect(put?.[0]).toBe('/app/networking');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ expected_revision: 1, service_ips: { web: '10.96.0.41' } });
  expect(new Headers(put?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
});

it.each([409, 500])('blocks uncertain saves across reopening and hides server text (%i)', async (status) => {
  vi.stubGlobal('fetch', vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? json({ error: 'secret-canary' }, status) : json(String(url).endsWith('/inventory') ? inventory : revision)));
  render(<KubernetesNetworking base="/app" org="a" instance={instance} expected={1} editable onSaved={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Save networking as revision 2' }));
  await screen.findByText(/The revision changed or the save outcome is unknown/);
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  expect((await screen.findByRole('button', { name: 'Save networking as revision 2' })).hasAttribute('disabled')).toBe(true);
  expect(document.body.textContent).not.toContain('secret-canary');
});

it('shows backend addresses without edit authority', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: RequestInfo | URL) => json(String(url).endsWith('/inventory') ? inventory : revision)));
  render(<KubernetesNetworking base="/app" org="a" instance={instance} expected={1} editable={false} onSaved={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  expect((await screen.findByLabelText('Internal IP for web')).hasAttribute('disabled')).toBe(true);
  expect(screen.queryByRole('button', { name: /Save networking/ })).toBeNull();
});

it('keeps edits across bounded pages and allows explicitly clearing saved IPs', async () => {
  const many = { ...revision, spec: { ...revision.spec, services: [...revision.spec.services, ...Array.from({ length: 24 }, (_, n) => ({ name: `app${n}`, image: 'nginx:1', ports: [{ target: 80, published: 80, protocol: 'tcp' }] }))] } };
  const saved = vi.fn();
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? json({ revision: 2 }, 201) : json(String(url).endsWith('/inventory') ? inventory : many));
  vi.stubGlobal('fetch', fetcher);
  render(<KubernetesNetworking base="/app" org="a" instance={instance} expected={1} editable onSaved={saved} />);
  fireEvent.click(screen.getByRole('button', { name: 'Internal networking' }));
  fireEvent.change(await screen.findByLabelText('Internal IP for web'), { target: { value: '' } });
  expect(screen.getAllByRole('row')).toHaveLength(26);
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  fireEvent.change(screen.getByLabelText('Internal IP for app23'), { target: { value: '10.96.0.50' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save networking as revision 2' }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  const put = fetcher.mock.calls.find(([, init]) => init?.method === 'PUT');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ expected_revision: 1, service_ips: { app23: '10.96.0.50' } });
});
