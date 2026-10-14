import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { AccountSecurity } from './AccountSecurity';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

type Route = (init?: RequestInit) => Response;
function serve(user: object, routes: Record<string, Route>) {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/auth/me') return new Response(JSON.stringify({ authenticated: true, user }));
    const route = routes[url];
    return route ? route(init) : new Response('{}', { status: 404 });
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const json = (body: object, status = 200) => new Response(JSON.stringify(body), { status });
function sent(fetcher: ReturnType<typeof serve>, url: string) {
  const call = fetcher.mock.calls.find(([u]) => u === url);
  if (!call?.[1]) throw new Error(`no request to ${url}`);
  return { body: JSON.parse(String(call[1].body)), csrf: new Headers(call[1].headers).get('X-CSRF-Token') };
}

it('enrols with the setup key, confirms with password and code, and shows recovery codes once', async () => {
  document.cookie = 'ky_csrf=proof';
  const fetcher = serve({ sso_provider: 'local', totp_enabled: false }, {
    '/api/account/mfa/enrol': () => json({ secret: 'JBSWY3DPEHPK3PXP', otpauth_uri: 'otpauth://totp/KyYard:alice?secret=JBSWY3DPEHPK3PXP' }),
    '/api/account/mfa/confirm': () => json({ totp_enabled: true, recovery_codes: ['aaaa-bbbb-cccc', 'dddd-eeee-ffff'] }),
  });
  render(<AccountSecurity />);
  fireEvent.click(await screen.findByRole('button', { name: 'Set up two-factor authentication' }));
  expect(await screen.findByLabelText('Setup key')).toHaveProperty('value', 'JBSWY3DPEHPK3PXP');
  expect(screen.getByRole('link', { name: 'Open in authenticator app' }).getAttribute('href')).toContain('otpauth://totp/');
  const confirm = within(screen.getByRole('form', { name: 'Turn on two-factor authentication' }));
  fireEvent.change(confirm.getByLabelText('Current password'), { target: { value: 'local-secret' } });
  fireEvent.change(confirm.getByLabelText('Authenticator code'), { target: { value: '123456' } });
  fireEvent.click(screen.getByRole('button', { name: 'Turn on two-factor authentication' }));
  const codes = await screen.findByRole('textbox', { name: 'Recovery codes' });
  expect(codes).toHaveProperty('value', 'aaaa-bbbb-cccc\ndddd-eeee-ffff');
  expect(sent(fetcher, '/api/account/mfa/enrol').csrf).toBe('proof');
  expect(sent(fetcher, '/api/account/mfa/confirm')).toEqual({ body: { password: 'local-secret', code: '123456' }, csrf: 'proof' });
  fireEvent.click(screen.getByRole('button', { name: 'I have saved these codes' }));
  expect(screen.queryByRole('textbox', { name: 'Recovery codes' })).toBeNull();
});

it('disables and replaces codes when enabled, and names refusals', async () => {
  document.cookie = 'ky_csrf=proof';
  const fetcher = serve({ sso_provider: 'local', totp_enabled: true }, {
    '/api/account/mfa/disable': () => json({ error: 'Invalid or already used code' }, 401),
    '/api/account/mfa/recovery-codes': () => json({ error: 'Too many attempts' }, 429),
  });
  render(<AccountSecurity />);
  const disable = await screen.findByRole('form', { name: 'Turn off two-factor authentication' });
  fireEvent.change(disable.querySelector('input[name=password]')!, { target: { value: 'local-secret' } });
  fireEvent.change(screen.getByLabelText('Authenticator or recovery code'), { target: { value: 'aaaa-bbbb-cccc' } });
  fireEvent.click(screen.getByRole('button', { name: 'Turn off two-factor authentication' }));
  await screen.findByText('Check your current password and code.');
  expect(sent(fetcher, '/api/account/mfa/disable')).toEqual({ body: { password: 'local-secret', code: 'aaaa-bbbb-cccc' }, csrf: 'proof' });
  expect(disable.querySelector('input[name=password]')).toHaveProperty('value', '');

  const replace = screen.getByRole('form', { name: 'Replace recovery codes' });
  fireEvent.change(replace.querySelector('input[name=code]')!, { target: { value: '654321' } });
  fireEvent.click(screen.getByRole('button', { name: 'Replace recovery codes' }));
  await screen.findByText('Too many attempts. Wait a few minutes and try again.');
  expect(sent(fetcher, '/api/account/mfa/recovery-codes').body).toEqual({ code: '654321' });
});

it('changes the password without sending the confirmation, and shows policy errors', async () => {
  const fetcher = serve({ sso_provider: 'local', totp_enabled: false }, {
    '/api/account/password': () => json({ error: 'password must be at least 12 characters' }, 400),
  });
  render(<AccountSecurity />);
  const form = await screen.findByRole('form', { name: 'Change password' });
  const field = (name: string, value: string) => fireEvent.change(form.querySelector(`input[name=${name}]`)!, { target: { value } });
  field('password', 'old-password-1');
  field('new_password', 'new-password-12');
  field('confirm_password', 'different-password');
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  await screen.findByText('The new passwords do not match.');
  expect(fetcher.mock.calls.some(([u]) => u === '/api/account/password')).toBe(false);

  field('confirm_password', 'new-password-12');
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  await screen.findByText('password must be at least 12 characters');
  expect(sent(fetcher, '/api/account/password').body).toEqual({ password: 'old-password-1', new_password: 'new-password-12' });
});

it('asks for a code on password change when enabled and reports forbidden accounts', async () => {
  const fetcher = serve({ sso_provider: 'local', totp_enabled: true }, { '/api/account/password': () => json({ error: 'no' }, 403) });
  render(<AccountSecurity />);
  const form = await screen.findByRole('form', { name: 'Change password' });
  const field = (name: string, value: string) => fireEvent.change(form.querySelector(`input[name=${name}]`)!, { target: { value } });
  field('password', 'old-password-1'); field('new_password', 'new-password-12'); field('confirm_password', 'new-password-12'); field('code', '111111');
  fireEvent.click(screen.getByRole('button', { name: 'Change password' }));
  await screen.findByText('This account cannot change its sign-in security here.');
  await waitFor(() => expect(sent(fetcher, '/api/account/password').body).toEqual({ password: 'old-password-1', new_password: 'new-password-12', code: '111111' }));
});

it('offers no local controls to a federated account', async () => {
  serve({ sso_provider: 'oidc', totp_enabled: false }, {});
  render(<AccountSecurity />);
  await screen.findByText(/signs in through its identity provider/);
  expect(screen.queryByRole('button')).toBeNull();
});
