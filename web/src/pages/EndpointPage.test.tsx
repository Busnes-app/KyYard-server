import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { EndpointPage } from './EndpointPage';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: { hostname: 'h1' }, fingerprint: 'ab'.repeat(32), capabilities: ['docker.containers'], alerts: [], created_at: '' };

it('distinguishes no inventory yet from an empty host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ error: 'nope' }, 404) : String(input).endsWith('/samples') ? json([]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText(/No inventory yet/)).toBeTruthy();
  expect(screen.queryByText('Containers')).toBeNull();
});

it('renders a fresh snapshot and flags staleness and truncation', async () => {
  const now = new Date();
  const old = new Date(now.getTime() - 10 * 60 * 1000).toISOString();
  const snapshot = { generation: 7, observed_at: old, engine: { runtime: 'docker', version: '29.7.2', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 8, memory_bytes: 2 ** 31, hostname: 'h1' }, containers: [{ id: 'c1', name: 'web', image: 'nginx:1', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: {}, networks: [], compose_project: 'shop' }], images: [], networks: [], volumes: [], truncated: ['images'] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', state: 'active', generation: 7, observed_at: old, received_at: old, snapshot }) : String(input).endsWith('/samples') ? json([{ container_id: 'c1', observed_at: old, cpu_percent: 12.34, memory_bytes: 3 * 2 ** 20, memory_limit: 0, rx_bytes: 0, tx_bytes: 0, pids: 1 }]) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText('cpu 12.3% · mem 3 MiB')).toBeTruthy();
  const status = await screen.findByRole('status');
  expect(status.textContent).toContain('generation 7');
  expect(status.textContent).toContain('stale');
  expect(status.textContent).toContain('truncated: images');
  expect(screen.getByText('8080→80/tcp')).toBeTruthy();
  expect(screen.getByText('No images on this host.')).toBeTruthy();
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
