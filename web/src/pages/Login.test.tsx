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
