import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Endpoints } from './Endpoints';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const pending = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'pending', facts: { hostname: 'h1' }, fingerprint: 'ab'.repeat(32), created_at: '2026-09-16T00:00:00Z' };

it('mints a one-time enrollment command with the socket disclosure', async () => {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') {
      expect(String(input)).toBe('/api/organizations/a/environments/env-a/enrollment-tokens');
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-test');
      return json({ id: 't1', runtime: 'docker', expires_at: '2026-09-16T00:15:00Z', token: 'tok', command: "printf 'tok' | docker run -i ...", disclosure: 'root-equivalent access' }, 201);
    }
    return json([]);
  }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  await screen.findByText(/No endpoints yet/);
  fireEvent.click(screen.getByRole('button', { name: 'Enroll a host' }));
  const region = await screen.findByRole('region', { name: 'Enrollment command' });
  expect(region.textContent).toContain('docker run -i');
  expect(region.textContent).toContain('root-equivalent access');
  expect(region.textContent).toContain('Shown once');
});

it('approves with the exact fingerprint after confirmation and hides actions for terminal states', async () => {
  const calls: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') {
      calls.push(String(input));
      expect(init.body).toBe(JSON.stringify({ fingerprint: pending.fingerprint }));
      return new Response(null, { status: 204 });
    }
    return json(calls.length ? [{ ...pending, state: 'revoked', fingerprint: '' }] : [pending]);
  }));
  vi.stubGlobal('confirm', vi.fn((msg: string) => { expect(msg).toContain(pending.fingerprint); return true; }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
  await screen.findByText('revoked');
  expect(calls).toEqual(['/api/organizations/a/endpoints/ep_1/approve']);
  expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull();
});
