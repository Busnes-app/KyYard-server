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

it('names the target host when identical container names occur on different hosts', async () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team', role: 'operator' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: [{ id: 'c', name: 'web', image: 'nginx:1', state: 'running', ports: [] }] } }) : json(['First host', 'Second host'].map((name, i) => ({ id: `e${i}`, name, state: 'active', runtime: 'docker', facts: {} })))));
  render(<Dashboard />);
  await screen.findAllByText('web');
  const hostSelect = screen.queryByRole('combobox', { name: 'Docker host' });
  if (hostSelect) fireEvent.change(hostSelect, { target: { value: 'e1' } });
  const buttons = await screen.findAllByRole('button', { name: 'Stop' });
  fireEvent.click(buttons[buttons.length - 1]);
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Second host'));
});

it('pages a large inventory and searches containers beyond the current page', async () => {
  let count = 61;
  vi.stubGlobal('fetch', vi.fn(async (url: string) => url === '/api/organizations' ? json([{ id: 'a', name: 'Team' }]) : url.includes('/inventory') ? json({ received_at: new Date().toISOString(), snapshot: { containers: Array.from({ length: count }, (_, i) => ({ id: `c${i}`, name: `container-${i}`, image: 'nginx:1', state: 'running', ports: [] })) } }) : json([{ id: 'e', name: 'Docker host', state: 'active', runtime: 'docker', facts: {} }])));
  render(<Dashboard />);
  await screen.findByText('container-0');
  expect(screen.getAllByRole('row')).toHaveLength(26);
  expect(screen.queryByText('container-25')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('container-25')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('51–61 of 61')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Next page' }).hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'container-0' } });
  expect(screen.getByText('container-0')).toBeTruthy();
  fireEvent.change(screen.getByRole('searchbox'), { target: { value: '' } });
  expect(screen.getByText('1–25 of 61')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  count = 2;
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByText('container-0');
  expect(screen.queryByRole('navigation', { name: 'Pagination' })).toBeNull();
});
