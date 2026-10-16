import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { InventoryRefresh } from './InventoryRefresh';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

it('waits for a fresh report and sends CSRF before reloading the list', async () => {
  document.cookie = 'ky_csrf=refresh-csrf';
  let finish: (response: Response) => void = () => {};
  const fetcher = vi.fn(() => new Promise<Response>((resolve) => { finish = resolve; }));
  vi.stubGlobal('fetch', fetcher);
  const reload = vi.fn();
  render(<InventoryRefresh base="/host" onRefresh={reload} />);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
  expect(screen.getByRole('button', { name: 'Refreshing inventory…' }).hasAttribute('disabled')).toBe(true);
  expect(reload).not.toHaveBeenCalled();
  expect(fetcher).toHaveBeenCalledWith('/host/inventory/refresh', expect.objectContaining({ method: 'POST', headers: expect.any(Headers) }));
  const init: RequestInit | undefined = vi.mocked(fetch).mock.calls[0][1];
  expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('refresh-csrf');
  finish(new Response('{}'));
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  expect(screen.getByRole('status').textContent).toBe('Inventory refreshed.');
});

it.each([409, 501, 504])('reports refresh failure %s without claiming the inventory changed', async (status) => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status })));
  const reload = vi.fn();
  render(<InventoryRefresh base="/host" onRefresh={reload} />);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
  await waitFor(() => expect(screen.getByRole('button').hasAttribute('disabled')).toBe(false));
  expect(reload).not.toHaveBeenCalled();
  expect(screen.getByRole('status').textContent).toMatch(/offline|Upgrade|No fresh/);
});

it('keeps an empty live region mounted before any refresh', () => {
  render(<InventoryRefresh base="/host" onRefresh={vi.fn()} />);
  expect(screen.getByRole('status').textContent).toBe('');
});

it.each([
  [401, 'Your session has expired. Sign in again.'],
  [403, 'You do not have permission to refresh this inventory.'],
  [429, 'Too many refresh requests. Try again in a minute.'],
])('names refusal %s with fixed text', async (status, text) => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('server says no', { status })));
  const reload = vi.fn();
  render(<InventoryRefresh base="/host" onRefresh={reload} />);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
  await waitFor(() => expect(screen.getByRole('status').textContent).toBe(text));
  expect(reload).not.toHaveBeenCalled();
  expect(screen.getByRole('button').hasAttribute('disabled')).toBe(false);
});

it('reports an unreachable server', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network'); }));
  render(<InventoryRefresh base="/host" onRefresh={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
  await waitFor(() => expect(screen.getByRole('status').textContent).toBe('The server could not be reached. Inventory was not refreshed.'));
});

it('aborts the request on unmount', () => {
  const fetcher = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => new Promise<Response>(() => {}));
  vi.stubGlobal('fetch', fetcher);
  const { unmount } = render(<InventoryRefresh base="/host" onRefresh={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
  const signal = fetcher.mock.calls[0][1]?.signal;
  expect(signal?.aborted).toBe(false);
  unmount();
  expect(signal?.aborted).toBe(true);
});

it('clears the refreshed message after a short time', async () => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{}')));
    render(<InventoryRefresh base="/host" onRefresh={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Refresh inventory' }));
    await vi.waitFor(() => expect(screen.getByRole('status').textContent).toBe('Inventory refreshed.'));
    act(() => { vi.advanceTimersByTime(10_000); });
    expect(screen.getByRole('status').textContent).toBe('');
  } finally { vi.useRealTimers(); }
});
