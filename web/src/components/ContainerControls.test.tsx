import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ContainerControls, ContainerLogs } from './ContainerControls';
import type { Container } from '../tenant';
const container: Container = { id: 'abc', name: 'web', image: 'nginx:1', image_id: 'sha256:123', state: 'running', status: 'Up', ports: [], networks: [], labels: {}, created_at: '' };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
it('dispatches the observed ID and state with CSRF and never automatically retries an unknown result', async () => {
  document.cookie = 'ky_csrf=test-token';
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify({ id: 'cmd', action: 'container.restart', outcome: 'unknown' }), { status: 202 }));
  vi.stubGlobal('fetch', fetcher);
  render(<ContainerControls base="/api/org/endpoint" container={container} active scope="Team / Production / Host" onRefresh={() => {}} canExec={false} />);
  fireEvent.click(screen.getByText('Actions'));
  fireEvent.click(screen.getByRole('button', { name: 'Restart' }));
  expect(await screen.findByText('container.restart: unknown')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
  const call = fetcher.mock.calls[0];
  expect(call?.[0]).toBe('/api/org/endpoint/commands');
  expect(JSON.parse(String(call?.[1]?.body))).toEqual({ action: 'container.restart', container: 'abc', confirm: '', expects: { state: 'running', image_digest: 'sha256:123' } });
  expect(new Headers(call?.[1]?.headers).get('X-CSRF-Token')).toBe('test-token');
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('Team / Production / Host'));
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
it('opens logs in a modal and closes the reader on Escape', () => {
 const show = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
 Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: show });
 render(<ContainerControls base="/api/org/endpoint" container={container} active scope="Team / Production / Host" onRefresh={() => {}} canExec={false} />);
 fireEvent.click(screen.getByText('Actions'));
 fireEvent.click(screen.getByRole('button', { name: 'Logs' }));
 const dialog = screen.getByRole('dialog', { name: 'Logs for web' });
 expect(show).toHaveBeenCalledTimes(1);
 fireEvent(dialog, new Event('cancel', { cancelable: true }));
 expect(screen.queryByRole('dialog')).toBeNull();
 Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal');
});
it.each([false, true])('offers Terminal only when the role may exec (%s)', (canExec) => {
  render(<ContainerControls base="/api/org/endpoint" container={container} active scope="Host" onRefresh={() => {}} canExec={canExec} />);
  fireEvent.click(screen.getByText('Actions'));
  expect(screen.queryByRole('button', { name: 'Terminal' }) !== null).toBe(canExec);
  expect(screen.getByRole('button', { name: 'Logs' })).toBeTruthy();
});
