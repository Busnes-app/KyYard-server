import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
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

it('hides the service tokens panel from an operator', async () => {
  stubAs('operator');
  render(<Members org="a" />);
  expect(await screen.findByText('admin')).toBeTruthy();
  expect(screen.queryByRole('heading', { name: 'Service tokens' })).toBeNull();
});

it('shows the service tokens panel to an organization administrator', async () => {
  stubAs('organization_admin');
  render(<Members org="a" />);
  expect(await screen.findByRole('heading', { name: 'Service tokens' })).toBeTruthy();
});
