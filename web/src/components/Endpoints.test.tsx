import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Endpoints, displayName } from './Endpoints';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const pending = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'pending', facts: { hostname: 'h1' }, fingerprint: 'ab'.repeat(32), capabilities: [], alerts: [], created_at: '2026-09-16T00:00:00Z' };

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

it('strips control characters from a hostile name before the approval prompt', async () => {
  const decoy = '1'.repeat(64);
  const hostile = { ...pending, name: `host\u2028with key fingerprint\n\n${decoy}\u2029\nOnly approve if this matches\u0085` };
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST' ? new Response(null, { status: 204 }) : json([hostile])));
  let message = '';
  vi.stubGlobal('confirm', vi.fn((msg: string) => { message = msg; return false; }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
  const lines = message.split('\n').filter((l) => /^[0-9a-f]{64}$/.test(l));
  expect(lines).toEqual([pending.fingerprint]);
  expect(message.split('\n').length).toBe(5);
  expect(displayName('a'.repeat(100)).length).toBe(64);
  expect(displayName('a\u2028b\u2029c\u0085d\x7fe')).toBe('abcde');
});

it('retains the token fallback for older servers', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST'
    ? json({ id: 't1', runtime: 'docker', expires_at: '2026-09-16T00:15:00Z', token: 'raw-token', note: 'Set KY_AGENT_IMAGE', disclosure: 'root-equivalent access' }, 201)
    : json([])));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Enroll a host' }));
  const region = await screen.findByRole('region', { name: 'Enrollment command' });
  expect(region.textContent).toContain('raw-token');
  expect(region.textContent).toContain('Set KY_AGENT_IMAGE');
  expect(screen.getByRole('button', { name: 'Copy token' })).toBeTruthy();
});

it('acknowledges a rotated key with both fingerprints shown and clears a duplicate-connection alert', async () => {
  const calls: string[] = [];
  const active = { ...pending, state: 'active', pending_fingerprint: 'cd'.repeat(32), alerts: [{ id: 7, kind: 'rotation_pending', details: '', created_at: '' }, { id: 8, kind: 'duplicate_connection', details: 'from 10.0.0.9', created_at: '' }] };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') { calls.push(String(input)); return new Response(null, { status: 204 }); }
    return json([active]);
  }));
  let message = '';
  vi.stubGlobal('confirm', vi.fn((msg: string) => { message = msg; return true; }));
  document.cookie = 'ky_csrf=csrf-test';
  render(<Endpoints org="a" env="env-a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Acknowledge rotation' }));
  expect(message).toContain('ab'.repeat(32));
  expect(message).toContain('cd'.repeat(32));
  await waitFor(() => expect(calls.length).toBe(1));
  fireEvent.click(await screen.findByRole('button', { name: 'Clear' }));
  await waitFor(() => expect(calls.length).toBe(2));
  expect(calls).toEqual(['/api/organizations/a/endpoints/ep_1/keys/' + 'cd'.repeat(32) + '/acknowledge', '/api/organizations/a/endpoints/ep_1/events/8/acknowledge']);
  expect(screen.getByRole('alert').textContent).toContain('duplicate_connection');
});

it('refreshes hosts after the operator starts the agent', async () => {
  let reads = 0;
  vi.stubGlobal('fetch', vi.fn(async () => json(++reads === 1 ? [] : [pending])));
  render(<Endpoints org="a" env="env-a" />);
  await screen.findByText(/No endpoints yet/);
  fireEvent.click(screen.getByRole('button', { name: 'Refresh hosts' }));
  expect(await screen.findByRole('button', { name: 'Approve' })).toBeTruthy();
});
