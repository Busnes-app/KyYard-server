import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { Administration } from './Administration';
import { Settings } from '../pages/Settings';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const acme = { id: 'org_1', name: 'Acme', created_at: '2026-09-01T10:00:00Z', members: 3 };
const alice = { id: 'usr_a', username: 'alice', display_name: 'Alice A', role: 'admin', status: 'active', sso_provider: 'local', created_at: '2026-09-01T10:00:00Z' };
const bob = { id: 'usr_b', username: 'bob', display_name: 'Bob B', role: 'user', status: 'suspended', sso_provider: 'oidc', created_at: '2026-09-02T10:00:00Z' };

type Write = { url: string; body: unknown; csrf: string | null };

// GETs read the lists; POSTs are recorded and answered with `reply`.
function serve(reply: (url: string) => Response) {
  const writes: Write[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if ((init?.method ?? 'GET') === 'GET') {
      if (url === '/api/admin/organizations') return json([acme]);
      if (url === '/api/admin/users') return json([alice, bob]);
      return json([]);
    }
    writes.push({ url, body: JSON.parse(String(init?.body)), csrf: new Headers(init?.headers).get('X-CSRF-Token') });
    return reply(url);
  }));
  document.cookie = 'ky_csrf=csrf-test';
  return writes;
}
const secret = (status: number, code: string) => json({ error: '<b>secret server detail</b>', code }, status);

it('shows the Administration section only to a platform admin', async () => {
  serve(() => json({}));
  const { unmount } = render(<Settings settings={null} role="user" />);
  expect(screen.queryByRole('button', { name: 'Administration' })).toBeNull();
  unmount();
  render(<Settings settings={null} role="admin" />);
  fireEvent.click(screen.getByRole('button', { name: 'Administration' }));
  expect(await screen.findByText('Acme')).toBeTruthy();
});

it('lists organizations with member counts and users with their status and provider', async () => {
  serve(() => json({}));
  render(<Administration />);
  const orgRow = (await screen.findByText('Acme')).closest('tr') as HTMLElement;
  expect(within(orgRow).getByText('3')).toBeTruthy();
  expect(within(orgRow).getByText('org_1').tagName).toBe('CODE');
  const bobRow = (await screen.findByText('bob')).closest('tr') as HTMLElement;
  expect(within(bobRow).getByText('Bob B')).toBeTruthy();
  expect(within(bobRow).getByText('usr_b').tagName).toBe('CODE');
  expect(within(bobRow).getByText('suspended')).toBeTruthy();
  expect(within(bobRow).getByText('oidc')).toBeTruthy();
});

it('offers only active users as the first administrator', async () => {
  serve(() => json({}));
  render(<Administration />);
  await screen.findByText('Acme');
  const picker = await screen.findByLabelText('First administrator') as HTMLSelectElement;
  const values = Array.from(picker.options).map((o) => o.value).filter(Boolean);
  expect(values).toEqual(['usr_a']);
});

it('creates an organization with CSRF and clears the form', async () => {
  const writes = serve(() => json({ id: 'org_2', name: 'Beta', created_at: '2026-09-24T00:00:00Z' }, 201));
  render(<Administration />);
  await screen.findByText('Acme');
  const name = screen.getByLabelText('Organization name') as HTMLInputElement;
  fireEvent.change(name, { target: { value: 'Beta' } });
  fireEvent.change(screen.getByLabelText('First administrator'), { target: { value: 'usr_a' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
  expect(await screen.findByText('Organization created. Its admin adds members on the organization page.')).toBeTruthy();
  expect(writes).toEqual([{ url: '/api/admin/organizations', body: { name: 'Beta', admin_user_id: 'usr_a' }, csrf: 'csrf-test' }]);
  expect(name.value).toBe('');
});

it.each([
  [409, 'organization_exists', 'An organization with that name already exists.'],
  [404, 'user_not_found', 'That user no longer exists. Refresh and choose another.'],
  [409, 'user_inactive', 'That user is not active. Choose an active user.'],
  [403, '', 'Administrator role required.'],
])('maps an organization refusal %i %s to fixed text', async (status, code, text) => {
  serve(() => secret(status, code));
  render(<Administration />);
  await screen.findByText('Acme');
  fireEvent.change(screen.getByLabelText('Organization name'), { target: { value: 'Acme' } });
  fireEvent.change(screen.getByLabelText('First administrator'), { target: { value: 'usr_a' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create organization' }));
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByText(/secret server detail/)).toBeNull();
});

it('creates a user, shows the temporary password once with Copy, and clears the form', async () => {
  const writes = serve(() => json({ id: 'usr_c', username: 'carol', display_name: 'Carol C', role: 'user', temporary_password: 'Tmp0123456789abcdefghijk' }, 201));
  const writeText = vi.fn(async () => {});
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
  render(<Administration />);
  await screen.findByText('bob');
  const username = screen.getByLabelText('Username') as HTMLInputElement;
  const display = screen.getByLabelText('Display name') as HTMLInputElement;
  fireEvent.change(username, { target: { value: 'carol' } });
  fireEvent.change(display, { target: { value: 'Carol C' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create user' }));
  const field = await screen.findByLabelText('Temporary password for carol') as HTMLInputElement;
  expect(field.readOnly).toBe(true);
  expect(field.value).toBe('Tmp0123456789abcdefghijk');
  expect(screen.getByText('Give this password to the person out of band. They must change it at first sign-in. It is not shown again.')).toBeTruthy();
  expect(writes).toEqual([{ url: '/api/admin/users', body: { username: 'carol', display_name: 'Carol C', role: 'user' }, csrf: 'csrf-test' }]);
  expect(username.value).toBe('');
  expect(display.value).toBe('');
  fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith('Tmp0123456789abcdefghijk'));
});

it.each([
  [409, 'username_exists', 'That username is already taken.'],
  [403, '', 'Administrator role required.'],
])('maps a user refusal %i %s to fixed text', async (status, code, text) => {
  serve(() => secret(status, code));
  render(<Administration />);
  await screen.findByText('bob');
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'bob' } });
  fireEvent.change(screen.getByLabelText('Display name'), { target: { value: 'Bob' } });
  fireEvent.click(screen.getByRole('button', { name: 'Create user' }));
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByText(/secret server detail/)).toBeNull();
  expect(screen.queryByLabelText(/Temporary password/)).toBeNull();
});
