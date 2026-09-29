import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ContainerControls, ContainerLogs } from './ContainerControls';
import type { Container } from '../tenant';
const id = 'a'.repeat(64);
const container: Container = { id, name: 'web', image: 'nginx:1', image_id: 'sha256:123', state: 'running', status: 'Up', ports: [], networks: [], labels: {}, created_at: '' };
const props = { base: '/api/org/endpoint', container, active: true, scope: 'Team / Production / Host', onRefresh: () => {}, org: 'org', endpoint: 'endpoint' };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it('dispatches the observed ID and state with CSRF and never automatically retries an unknown result', async () => {
  document.cookie = 'ky_csrf=test-token';
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify({ id: 'cmd', action: 'container.restart', outcome: 'unknown' }), { status: 202 }));
  vi.stubGlobal('fetch', fetcher);
  render(<ContainerControls {...props} canExec={false} />);
  fireEvent.click(screen.getByRole('button', { name: 'Restart web' }));
  expect(await screen.findByText('container.restart: unknown')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
  const call = fetcher.mock.calls[0];
  expect(call?.[0]).toBe('/api/org/endpoint/commands');
  expect(JSON.parse(String(call?.[1]?.body))).toEqual({ action: 'container.restart', container: id, confirm: '', expects: { state: 'running', image_digest: 'sha256:123' } });
  expect(new Headers(call?.[1]?.headers).get('X-CSRF-Token')).toBe('test-token');
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('Team / Production / Host'));
});

it('renders every action as a labelled icon control and adds no expandable content', () => {
  render(<ContainerControls {...props} canExec />);
  const group = screen.getByRole('group', { name: 'Actions for web' });
  for (const name of ['Stop web', 'Restart web', 'Remove web']) expect(screen.getByRole('button', { name })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Logs for web' }).getAttribute('href')).toBe(`/organizations/org/endpoints/endpoint/containers/${id}?tab=logs`);
  expect(screen.getByRole('link', { name: 'Terminal for web' }).getAttribute('href')).toBe(`/organizations/org/endpoints/endpoint/containers/${id}?tab=terminal`);
  expect(screen.queryByRole('button', { name: 'Start web' })).toBeNull();
  expect(group.querySelector('details, dialog')).toBeNull();
  expect(screen.queryByText('Actions')).toBeNull();
});

it('shows Start for a stopped container and disables Remove while running', () => {
  render(<ContainerControls {...props} container={{ ...container, state: 'exited' }} canExec={false} />);
  expect(screen.getByRole('button', { name: 'Start web' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Stop web' })).toBeNull();
  expect((screen.getByRole('button', { name: 'Remove web' }) as HTMLButtonElement).disabled).toBe(false);
  cleanup();
  render(<ContainerControls {...props} canExec={false} />);
  expect((screen.getByRole('button', { name: 'Remove web' }) as HTMLButtonElement).disabled).toBe(true);
});

it.each([false, true])('offers the Terminal link only when the role may exec (%s)', (canExec) => {
  render(<ContainerControls {...props} canExec={canExec} />);
  expect(screen.queryByRole('link', { name: 'Terminal for web' }) !== null).toBe(canExec);
  expect(screen.getByRole('link', { name: 'Logs for web' })).toBeTruthy();
});

it('reports status through onStatus instead of inline when given', async () => {
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ id: 'cmd', action: 'container.stop', outcome: 'succeeded' }), { status: 200 })));
  const onStatus = vi.fn();
  render(<ContainerControls {...props} canExec={false} onStatus={onStatus} />);
  fireEvent.click(screen.getByRole('button', { name: 'Stop web' }));
  await waitFor(() => expect(onStatus).toHaveBeenCalledWith('container.stop: succeeded'));
  expect(screen.queryByRole('status')).toBeNull();
});

it('bounds the browser log display and aborts its request on unmount', async () => {
  const fetcher = vi.fn(async () => new Response('x'.repeat(300000)));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<ContainerLogs url="/api/logs" name="web" />);
  fireEvent.click(screen.getByRole('button', { name: 'Load logs' }));
  await waitFor(() => expect(view.container.querySelector('pre')?.textContent?.length).toBe(262144));
  expect(screen.getByRole('status').textContent).toContain('last 256 KiB');
  view.unmount();
});
