import { StrictMode } from 'react';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { Login } from './Login';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.history.replaceState(null, '', '/'); });

it('keeps the SSO MFA challenge through StrictMode and clears browser history', async () => {
  const token = 'a'.repeat(64);
  window.history.replaceState(null, '', '/#sso-mfa=' + token);
  const request = vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, user: { id: 'local-admin' } }), { status: 200 }));
  vi.stubGlobal('fetch', request);
  const success = vi.fn();
  render(<StrictMode><Login appName="KyYard" onSuccess={success} /></StrictMode>);
  expect(screen.getByText('Two-Factor Verification')).toBeTruthy();
  expect(window.location.hash).toBe('');
  fireEvent.change(screen.getByPlaceholderText('123456'), { target: { value: '123456' } });
  fireEvent.submit(screen.getByPlaceholderText('123456').closest('form')!);
  await waitFor(() => expect(success).toHaveBeenCalledWith({ id: 'local-admin' }));
  expect(request).toHaveBeenCalledWith('/api/auth/mfa/totp', expect.objectContaining({ body: JSON.stringify({ mfa_token: token, code: '123456' }) }));
});

it('solves one challenge while typing and a fresh one after a refused login', async () => {
  const salt = 'ab';
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(salt + '3')));
  const challenge = Array.from(digest, (b) => b.toString(16).padStart(2, '0')).join('');
  const tokens: string[] = [];
  let issued = 0;
  const request = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/auth/pow-challenge') {
      issued++;
      return new Response(JSON.stringify({ algorithm: 'SHA-256', salt, challenge, maxnumber: 10, expires_at: issued, signature: 'sig' + issued }));
    }
    tokens.push(JSON.parse(String(init?.body)).captcha_token);
    return new Response(JSON.stringify({ error: 'Invalid credentials' }), { status: 401 });
  });
  vi.stubGlobal('fetch', request);
  render(<Login appName="KyYard" onSuccess={vi.fn()} />);
  await screen.findByText('Security check verified.');
  for (const value of ['a', 'ad', 'admin']) fireEvent.change(screen.getByLabelText('Username'), { target: { value } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'wrong-password' } });
  expect(issued).toBe(1);

  fireEvent.submit(screen.getByLabelText('Password').closest('form')!);
  await screen.findByText('Invalid credentials');
  await waitFor(() => expect(issued).toBe(2));
  await screen.findByText('Security check verified.');
  fireEvent.submit(screen.getByLabelText('Password').closest('form')!);
  await waitFor(() => expect(tokens).toHaveLength(2));
  expect(JSON.parse(atob(tokens[0])).signature).toBe('sig1');
  expect(JSON.parse(atob(tokens[1])).signature).toBe('sig2');
});
