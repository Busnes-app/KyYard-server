import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ChangePassword } from './ChangePassword';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

it('requires matching passwords and submits with CSRF before returning to login', async () => {
  const done = vi.fn(); const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('csrf-test');
    return new Response('{}', { status: 200 });
  });
  vi.stubGlobal('fetch', fetcher); document.cookie = 'ky_csrf=csrf-test';
  render(<ChangePassword onComplete={done} onLogout={vi.fn()} />);
  fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'TemporaryPassword123!' } });
  fireEvent.change(screen.getByLabelText('New password'), { target: { value: 'ReplacementPassword456!' } });
  fireEvent.change(screen.getByLabelText('Confirm new password'), { target: { value: 'DifferentPassword789!' } });
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  expect(screen.getByRole('alert').textContent).toContain('do not match'); expect(fetcher).not.toHaveBeenCalled();
  fireEvent.change(screen.getByLabelText('Confirm new password'), { target: { value: 'ReplacementPassword456!' } });
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  await waitFor(() => expect(done).toHaveBeenCalledOnce());
  expect(fetcher).toHaveBeenCalledWith('/api/auth/change-password', expect.objectContaining({ method: 'POST', body: JSON.stringify({ current_password: 'TemporaryPassword123!', new_password: 'ReplacementPassword456!' }) }));
});

it('keeps the restricted form available after a server rejection', async () => {
  const done = vi.fn(); vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: 'Current password is incorrect' }), { status: 401 })));
  render(<ChangePassword onComplete={done} onLogout={vi.fn()} />);
  for (const [label, value] of [['Current password', 'TemporaryPassword123!'], ['New password', 'ReplacementPassword456!'], ['Confirm new password', 'ReplacementPassword456!']]) {
    fireEvent.change(screen.getByLabelText(label), { target: { value } });
  }
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Current password is incorrect');
  expect(done).not.toHaveBeenCalled();
});

it('routes a restricted signed-in user to replacement instead of the dashboard', async () => {
  const { App } = await import('../App');
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(
    String(input) === '/api/auth/me'
      ? { authenticated: true, user: { username: 'admin', must_change_password: true } }
      : { app_name: 'Test Server', theme: 'patina' },
  ), { status: 200 })));
  render(<App />);
  expect(await screen.findByRole('heading', { name: 'Change your password' })).toBeTruthy();
  expect(screen.queryByText('Directory & SCIM')).toBeNull();
  expect(screen.queryByText('Overview')).toBeNull();
});
