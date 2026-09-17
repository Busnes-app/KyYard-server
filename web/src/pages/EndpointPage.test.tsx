import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { EndpointPage } from './EndpointPage';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: { hostname: 'h1' }, fingerprint: 'ab'.repeat(32), capabilities: ['docker.containers'], alerts: [], created_at: '' };

it('distinguishes no inventory yet from an empty host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ error: 'nope' }, 404) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  expect(await screen.findByText(/No inventory yet/)).toBeTruthy();
  expect(screen.queryByText('Containers')).toBeNull();
});

it('renders a fresh snapshot and flags staleness and truncation', async () => {
  const now = new Date();
  const old = new Date(now.getTime() - 10 * 60 * 1000).toISOString();
  const snapshot = { generation: 7, observed_at: old, engine: { runtime: 'docker', version: '29.7.2', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 8, memory_bytes: 2 ** 31, hostname: 'h1' }, containers: [{ id: 'c1', name: 'web', image: 'nginx:1', image_id: 'i', state: 'running', status: 'Up', created_at: '', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: {}, networks: [], compose_project: 'shop' }], images: [], networks: [], volumes: [], truncated: ['images'] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/inventory') ? json({ endpoint_id: 'ep_1', state: 'active', generation: 7, observed_at: old, received_at: old, snapshot }) : json(endpoint)));
  render(<EndpointPage org="a" endpoint="ep_1" />);
  const status = await screen.findByRole('status');
  expect(status.textContent).toContain('generation 7');
  expect(status.textContent).toContain('stale');
  expect(status.textContent).toContain('truncated: images');
  expect(screen.getByText('8080→80/tcp')).toBeTruthy();
  expect(screen.getByText('No images on this host.')).toBeTruthy();
  expect(screen.getByText('8 CPUs · 2.0 GiB')).toBeTruthy();
});
