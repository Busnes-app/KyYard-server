import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { AccountSSO } from './AccountSSO';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
it('proves the local password with CSRF and clears it after a refused attempt', async () => {
  document.cookie = 'ky_csrf=proof';
  const fetcher = vi.fn(async (url: string, _options?: RequestInit) => url === '/api/auth/sso' ? new Response(JSON.stringify([{ id: 'idp_test', name: 'KyIdentity' }])) : new Response('{}', { status: 401 }));
  vi.stubGlobal('fetch', fetcher);
  render(<AccountSSO />);
  const input = await screen.findByLabelText('Current KyYard password');
  fireEvent.change(input, { target: { value: 'local-secret' } });
  fireEvent.click(screen.getByRole('button', { name: 'Connect single sign-on' }));
  await screen.findByText('Check your current KyYard password and sign-in session.');
  expect(input).toHaveProperty('value', '');
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  const request = fetcher.mock.calls[1];
  expect(request[0]).toBe('/api/sso/idp_test/link');
  const options = request[1];
  expect(options).toMatchObject({ method: 'POST', body: JSON.stringify({ password: 'local-secret' }) });
  if (!options) throw new Error('Missing request headers');
  expect(new Headers(options.headers).get('X-CSRF-Token')).toBe('proof');
});
