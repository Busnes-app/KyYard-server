import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Dashboard } from './Dashboard';
const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('puts container inventory on the home page and links to real endpoint operations', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'web', image: 'nginx:1', state: 'running', ports: [] }] } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  expect(await screen.findByText('web')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Logs' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Stop' })).toBeTruthy();
  expect(screen.queryByRole('combobox')).toBeNull();
  expect(screen.queryByText(/Directory|Feature 0|Pluggable/)).toBeNull();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'missing' } });
  expect(screen.getByText('No matching containers on this host.')).toBeTruthy();
});
it('does not invent an empty fleet when organization access is denied', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 403 })));
  render(<Dashboard />);
  expect(await screen.findByRole('alert')).toBeTruthy();
  expect(screen.queryByText(/No endpoints here/)).toBeNull();
});

it('reports unavailable Docker instead of an empty host', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { engine: { version: '' }, containers: [] } }) : json([{ id: 'e', name: 'Local Docker', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  expect(await screen.findByText(/Docker is unavailable/)).toBeTruthy();
  expect(screen.queryByText('No containers on this host.')).toBeNull();
});
