import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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
