import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Members } from './Members';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

function stubAs(role: string) {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Org A', role }]);
    if (url === '/api/organizations/a/members') return json([{ user_id: 'usr_admin', username: 'admin', role, status: 'active' }]);
    if (url === '/api/organizations/a/service-tokens') return json([]);
    return json({ error: 'not found' }, 404);
  }));
}

it('lists people and links to Access, without the integration panels', async () => {
  stubAs('organization_admin');
  render(<Members org="a" />);
  expect(await screen.findByText('admin')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Access' }).getAttribute('href')).toBe('/organizations/a/access');
  expect(screen.queryByRole('heading', { name: 'Service tokens' })).toBeNull();
  expect(screen.queryByRole('heading', { name: 'Registries' })).toBeNull();
});

it('asks before a change that ends an administrator and revokes what they created', async () => {
  stubAs('organization_admin');
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
  render(<Members org="a" />);
  fireEvent.change(await screen.findByRole('combobox', { name: 'Role for admin' }), { target: { value: 'operator' } });
  expect(confirm).toHaveBeenLastCalledWith(expect.stringMatching(/admin stops being an administrator\. The AI agent tokens and status feeds they created are revoked/));
  fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
  expect(confirm).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
  expect(confirm).toHaveBeenLastCalledWith(expect.stringMatching(/^Remove admin and revoke their access\? The AI agent tokens/));
  const fetch = vi.mocked(globalThis.fetch);
  expect(fetch.mock.calls.some(([, init]) => init?.method === 'PUT' || init?.method === 'DELETE')).toBe(false);
  confirm.mockRestore();
});

it('changes a non-administrator without asking', async () => {
  stubAs('operator');
  const confirm = vi.spyOn(window, 'confirm');
  render(<Members org="a" />);
  fireEvent.change(await screen.findByRole('combobox', { name: 'Role for admin' }), { target: { value: 'read_only' } });
  expect(confirm).not.toHaveBeenCalled();
  confirm.mockRestore();
});
