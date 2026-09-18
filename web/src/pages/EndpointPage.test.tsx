import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { EndpointPage } from './EndpointPage';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: { hostname: 'h1' }, fingerprint: 'ab'.repeat(32), capabilities: ['docker.containers'], alerts: [], created_at: '' };

it('distinguishes no inventory yet from an empty host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ error: 'nope' }, 404) : String(input).endsWith('/samples') ? json([]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText(/No inventory yet/)).toBeTruthy();
  expect(screen.queryByRole('table')).toBeNull();
});

it('renders a fresh snapshot and flags staleness and truncation', async () => {
  const now = new Date();
  const old = new Date(now.getTime() - 10 * 60 * 1000).toISOString();
  const snapshot = { generation: 7, observed_at: old, engine: { runtime: 'docker', version: '29.7.2', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 8, memory_bytes: 2 ** 31, hostname: 'h1' }, containers: [{ id: 'c1', name: 'web', image: 'nginx:1', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: {}, networks: [], compose_project: 'shop' }], images: [], networks: [], volumes: [], truncated: ['images'] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', state: 'active', generation: 7, observed_at: old, received_at: old, snapshot }) : String(input).endsWith('/samples') ? json([{ container_id: 'c1', observed_at: old, cpu_percent: 12.34, memory_bytes: 3 * 2 ** 20, memory_limit: 0, rx_bytes: 0, tx_bytes: 0, pids: 1, restart_count: 3 }]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('cpu 12.3% · mem 3 MiB · 3 restarts')).toBeTruthy();
  const status = await screen.findByRole('status');
  expect(status.textContent).toContain('generation 7');
  expect(status.textContent).toContain('stale');
  expect(status.textContent).toContain('truncated: images');
  expect(screen.getByText(/8080→80\/tcp/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Images' }));
  expect(screen.getByText('No images on this host.')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Details' }));
  expect(screen.getByText('8 CPUs · 2.0 GiB')).toBeTruthy();
});

it('tells no data from zero usage', async () => {
  const snapshot = { generation: 1, observed_at: new Date().toISOString(), engine: { runtime: 'docker', version: '1', api_version: '1', os: 'linux', arch: 'x', kernel: 'k', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers: [{ id: 'c1', name: 'a', image: 'i', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: [] }, { id: 'c2', name: 'b', image: 'i', image_id: 'i', state: 'exited', status: 'Exited', created_at: '', ports: [], labels: {}, networks: [] }, { id: 'c3', name: 'c', image: 'i', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: [] }], images: [], networks: [], volumes: [] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: snapshot.observed_at, received_at: snapshot.observed_at, snapshot }) : String(input).endsWith('/samples') ? json([{ container_id: 'c3', observed_at: snapshot.observed_at, cpu_percent: -1, memory_bytes: 0, memory_limit: 0, rx_bytes: 0, tx_bytes: 0, pids: 1 }]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('no data')).toBeTruthy(); // running, never sampled
  expect(screen.getAllByText('—').length).toBeGreaterThan(0); // exited container shows a dash
  expect(screen.getByText('cpu — · mem 0 B')).toBeTruthy(); // first sample: no interval yet, memory really zero
});

it('says nothing about restarts when the runtime would not say', async () => {
  const now = new Date().toISOString();
  const snapshot = { generation: 2, observed_at: now, engine: { runtime: 'docker', version: '1', api_version: '1', os: 'linux', arch: 'x', kernel: 'k', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers: [{ id: 'c1', name: 'a', image: 'i', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: [] }], images: [], networks: [], volumes: [], truncated: [] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', state: 'active', generation: 2, observed_at: now, received_at: now, snapshot }) : String(input).endsWith('/samples') ? json([{ container_id: 'c1', observed_at: now, cpu_percent: 5, memory_bytes: 2 ** 20, memory_limit: 0, rx_bytes: 0, tx_bytes: 0, pids: 1, restart_count: -1 }]) : json({ id: 'ep_1', name: 'h', state: 'active', environment_id: 'env', runtime: 'docker', fingerprint: 'f', alerts: [], capabilities: [] })));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  // A count of zero would be a claim; -1 is the runtime declining to answer.
  expect(await screen.findByText('cpu 5.0% · mem 1 MiB')).toBeTruthy();
});

it('discovers unmanaged projects, filters existing controls, and resets scope on navigation', async () => {
  const now = new Date().toISOString();
  const container = (id: string, name: string, project: string, state = 'running') => ({ id, name, image: 'alpine:3', image_id: 'i', state, status: state, created_at: now, ports: [], labels: {}, networks: [], compose_project: project });
  const containers = [container('c1', 'shop-web', 'shop'), container('c2', 'shop-db', 'shop', 'exited'), container('c3', 'mail-web', 'mail'), container('c4', 'standalone', '')];
  const snapshot = { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '1', api_version: '1', os: 'linux', arch: 'x', kernel: 'k', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [], truncated: ['containers'] };
  const fetcher = vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', generation: 1, observed_at: now, received_at: now, snapshot }) : String(input).endsWith('/samples') || String(input).includes('/commands') ? json([]) : json(endpoint));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<EndpointPage org="a" endpoint="ep_1" />);
  fireEvent.click(screen.getByRole('button', { name: 'Projects' }));
  const projects = await screen.findByRole('region', { name: 'Compose projects' });
  expect(projects.textContent).toContain('Compose projects (2)');
  expect(projects.textContent).toContain('1/2 containers running');
  expect(projects.textContent).toContain('Unmanaged');
  expect(projects.textContent).toContain('counts may be incomplete');
  fireEvent.click(within(projects).getByTitle('shop'));
  fireEvent.click(within(projects).getByRole('button', { name: 'Show containers for shop' }));
  const table = document.getElementById('endpoint-containers');
  if (!table) throw new Error('missing container region');
  expect(table.textContent).toContain('shop-web');
  expect(table.textContent).not.toContain('mail-web');
  expect(table.textContent).not.toContain('standalone');
  await waitFor(() => expect(document.activeElement).toBe(table));
  fireEvent.click(screen.getByRole('button', { name: 'Show all containers' }));
  expect(table.textContent).toContain('standalone');
  fireEvent.click(screen.getByRole('button', { name: 'Projects' }));
  fireEvent.click(within(screen.getByRole('region', { name: 'Compose projects' })).getByRole('button', { name: 'Show containers for shop', hidden: true }));
  view.rerender(<EndpointPage org="a" endpoint="ep_2" />);
  await screen.findByText('mail-web');
  expect(screen.queryByRole('button', { name: 'Show all containers' })).toBeNull();
  expect(document.getElementById('endpoint-containers')?.textContent).toContain('mail-web');
  expect(fetcher.mock.calls.every((call) => call.length === 1)).toBe(true); // discovery only reads inventory
});

it('pages host containers and resets pagination when searching', async () => {
  const now = new Date().toISOString();
  const containers = Array.from({ length: 52 }, (_, i) => ({ id: `c${i}`, name: `container-${i}`, image: 'alpine:3', state: 'running', ports: [] }));
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url.endsWith('/inventory') ? json({ generation: 1, received_at: now, observed_at: now, snapshot: { containers, engine: {}, images: [], networks: [], volumes: [] } }) : url.endsWith('/samples') || url.includes('/commands') ? json([]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  await screen.findByText('container-0');
  expect(screen.getAllByRole('row')).toHaveLength(26);
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('container-25')).toBeTruthy();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'container-51' } });
  expect(screen.getByText('container-51')).toBeTruthy();
  expect(screen.getAllByRole('row')).toHaveLength(2);
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: '' } });
  expect(screen.getByText('1–25 of 52')).toBeTruthy();
});
