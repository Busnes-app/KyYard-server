import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Organization } from './Organization';
import { Members } from './Members';
import { App } from '../App';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; window.history.replaceState(null, '', '/'); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

it('shows the denied state without any tenant data', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'Tenant access denied', code: 'tenant_access_denied' }, 403)));
  render(<Organization org="secret" />);
  expect((await screen.findByRole('alert')).textContent).toContain('do not have access');
  expect(screen.queryByText('Environments')).toBeNull();
});

it('shows the offline state with retry, then recovers', async () => {
  let calls = 0;
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    calls++;
    if (calls <= 2) throw new TypeError('Failed to fetch');
    return String(input).endsWith('/environments') ? json([]) : json({ id: 'a', name: 'Org A', role: 'organization_admin' });
  }));
  render(<Organization org="a" />);
  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toContain('Offline');
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByRole('heading', { name: 'Org A' })).toBeTruthy();
  expect(await screen.findByText(/No environments yet/)).toBeTruthy();
});

it('creates an environment with CSRF and reloads the list', async () => {
  const envs: { id: string; organization_id: string; name: string }[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === 'POST') {
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-test');
      expect(init.body).toBe(JSON.stringify({ name: 'Staging' }));
      envs.push({ id: 'env-1', organization_id: 'a', name: 'Staging' });
      return json(envs[0], 201);
    }
    return url.endsWith('/environments') ? json([...envs]) : json({ id: 'a', name: 'Org A', role: 'organization_admin' });
  }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Organization org="a" />);
  await screen.findByText(/No environments yet/);
  fireEvent.change(screen.getByLabelText('New environment'), { target: { value: 'Staging' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create' }));
  const link = await screen.findByRole('link', { name: 'Staging' });
  expect(link.getAttribute('href')).toBe('/organizations/a/environments/env-1');
});

it('surfaces the last-administrator refusal on the members screen', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PUT') return json({ error: 'The organization needs at least one active administrator', code: 'last_administrator' }, 409);
    return json([{ user_id: 'usr_admin', username: 'admin', role: 'organization_admin', status: 'active' }]);
  }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Members org="a" />);
  fireEvent.change(await screen.findByLabelText('Role for admin'), { target: { value: 'read_only' } });
  expect((await screen.findByRole('alert')).textContent).toContain('at least one active administrator');
});

it('opens a deep link after sign-in and navigates with history', async () => {
  window.history.replaceState(null, '', '/organizations/a/members');
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/auth/me') return json({ authenticated: true, user: { id: 'usr_admin', username: 'admin', role: 'user' } });
    if (url === '/api/settings') return json({ app_name: 'Test Server' });
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Org A', role: 'organization_admin' }]);
    if (url === '/api/organizations/a/members') return json([]);
    if (url === '/api/organizations/a') return json({ id: 'a', name: 'Org A' });
    if (url === '/api/organizations/a/environments') return json([]);
    return json({ error: 'not found' }, 404);
  }));
  render(<App />);
  expect(await screen.findByRole('heading', { name: 'Members' })).toBeTruthy();
  expect(await screen.findByRole('link', { name: 'Org A' })).toHaveProperty('pathname', '/organizations/a');
  expect(screen.queryByLabelText('Organization')).toBeNull();
  fireEvent.click(screen.getByRole('link', { name: 'Back to organization' }));
  expect(await screen.findByRole('heading', { name: 'Org A' })).toBeTruthy();
  expect(window.location.pathname).toBe('/organizations/a');
  window.history.back();
  window.dispatchEvent(new PopStateEvent('popstate'));
  await waitFor(() => expect(screen.getByRole('heading', { name: 'Members' })).toBeTruthy());
  expect(screen.getByRole('link', { name: 'Containers' }).getAttribute('aria-current')).toBeNull();
});
