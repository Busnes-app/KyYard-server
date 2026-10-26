import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Access } from './Access';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

function stubAs(role: string) {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Org A', role }]);
        if (url === '/api/organizations/a/service-tokens') return json([]);
    return json({ error: 'not found' }, 404);
  }));
}

it('hides the service tokens panel from an operator', async () => {
  stubAs('operator');
  render(<Access org="a" />);
  expect(await screen.findByRole('heading', { name: 'Access' })).toBeTruthy();
  expect(screen.queryByRole('heading', { name: 'Service tokens' })).toBeNull();
});

// Pair kyPulse and registry access live here, not under Members (people only).
it('shows service tokens with Pair kyPulse to an organization administrator', async () => {
  stubAs('organization_admin');
  render(<Access org="a" />);
  expect(await screen.findByRole('heading', { name: 'Service tokens' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Pair kyPulse' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Members' }).getAttribute('href')).toBe('/organizations/a/members');
});
