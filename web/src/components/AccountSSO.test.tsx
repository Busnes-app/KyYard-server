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
  await waitFor(() => expect(fetcher.mock.calls.some(c => c[0] === '/api/sso/idp_test/link')).toBe(true));
  const options = fetcher.mock.calls.find(c => c[0] === '/api/sso/idp_test/link')?.[1];
  expect(options).toMatchObject({ method: 'POST', body: JSON.stringify({ password: 'local-secret' }) });
  if (!options) throw new Error('Missing request headers');
  expect(new Headers(options.headers).get('X-CSRF-Token')).toBe('proof');
});

it('lists a linked identity and unlinks it with the password and CSRF', async () => {
  document.cookie = 'ky_csrf=proof';
  let linked = true;
  const fetcher = vi.fn(async (url: string, _options?: RequestInit) => {
    if (url === '/api/auth/sso') return new Response(JSON.stringify([{ id: 'idp_test', name: 'KyIdentity' }]));
    if (url === '/api/account/sso') return new Response(JSON.stringify(linked ? [{ provider: 'idp_test', name: 'KyIdentity', subject: 'stranger', linked_at: '2026-11-01T10:00:00Z' }] : []));
    linked = false;
    return new Response('{"unlinked":true}');
  });
  vi.stubGlobal('fetch', fetcher);
  render(<AccountSSO />);
  fireEvent.click(await screen.findByRole('button', { name: 'Unlink KyIdentity' }));
  const inputs = screen.getAllByLabelText('Current KyYard password');
  fireEvent.change(inputs[0], { target: { value: 'local-secret' } });
  fireEvent.click(screen.getByRole('button', { name: 'Unlink KyIdentity' }));
  await screen.findByText('KyIdentity sign-in unlinked. Sessions it started have ended.');
  const request = fetcher.mock.calls.find(c => c[0] === '/api/account/sso/idp_test/unlink');
  if (!request?.[1]) throw new Error('no unlink request');
  expect(request[1]).toMatchObject({ method: 'POST', body: JSON.stringify({ password: 'local-secret', code: '' }) });
  expect(new Headers(request[1].headers).get('X-CSRF-Token')).toBe('proof');
  await waitFor(() => expect(screen.queryByText(/stranger/)).toBeNull());
});
