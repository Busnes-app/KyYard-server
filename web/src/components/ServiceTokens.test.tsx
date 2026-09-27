import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ServiceTokens } from './ServiceTokens';

const tokens = [{ id: 'svc_1', organization_id: 'org_a', name: 'kypulse', created_by: 'pairing:p1', created_at: '2026-09-27T10:00:00Z', last_used_at: '2026-09-27T11:00:00Z', last_ip: '10.0.0.5' }];

function stub(extra: Array<[RegExp, unknown, number?]> = []) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    for (const [re, body, status] of extra) if (re.test(key)) return new Response(status === 204 ? null : JSON.stringify(body), { status: status ?? 200 });
    if (key === 'GET /api/organizations/org_a/service-tokens') return new Response(JSON.stringify(tokens));
    throw new Error(key);
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('ServiceTokens', () => {
  it('lists tokens and shows a pairing code once', async () => {
    stub([[/^POST \/api\/organizations\/org_a\/service-tokens\/pairings$/, { id: 'p2', code: '123456', expires_at: '2026-09-27T12:15:00Z', disclosure: 'Shown once.' }, 201]]);
    render(<ServiceTokens org="org_a" />);
    expect(await screen.findByText('kypulse')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Pair kyPulse' }));
    expect(await screen.findByText('123456')).toBeTruthy();
    expect(screen.getByText(/Shown once/)).toBeTruthy();
  });

  it('revokes after confirmation and names the kyPulse side', async () => {
    const fetchMock = stub([[/^DELETE \/api\/organizations\/org_a\/service-tokens\/svc_1$/, null, 204]]);
    vi.stubGlobal('confirm', vi.fn(() => true));
    render(<ServiceTokens org="org_a" />);
    await screen.findByText('kypulse');
    expect(screen.getByText(/Unpair in kyPulse as well/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Revoke' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit)?.method === 'DELETE')).toBe(true));
  });

  it('reloads the list when the code is dismissed', async () => {
    const fetchMock = stub([[/^POST \/api\/organizations\/org_a\/service-tokens\/pairings$/, { id: 'p2', code: '123456', expires_at: new Date(Date.now() + 900_000).toISOString(), disclosure: 'Shown once.' }, 201]]);
    render(<ServiceTokens org="org_a" />);
    await screen.findByText('kypulse');
    fireEvent.click(screen.getByRole('button', { name: 'Pair kyPulse' }));
    await screen.findByText('123456');
    const lists = () => fetchMock.mock.calls.filter(([input, init]) => String(input).endsWith('/service-tokens') && !(init as RequestInit)?.method).length;
    const before = lists();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    await waitFor(() => expect(lists()).toBeGreaterThan(before));
    expect(screen.queryByText('123456')).toBeNull();
  });

  it('drops the code and reloads the list when it expires', async () => {
    const fetchMock = stub([[/^POST \/api\/organizations\/org_a\/service-tokens\/pairings$/, { id: 'p2', code: '123456', expires_at: new Date(Date.now() + 50).toISOString(), disclosure: 'Shown once.' }, 201]]);
    render(<ServiceTokens org="org_a" />);
    await screen.findByText('kypulse');
    fireEvent.click(screen.getByRole('button', { name: 'Pair kyPulse' }));
    await screen.findByText('123456');
    const lists = () => fetchMock.mock.calls.filter(([input, init]) => String(input).endsWith('/service-tokens') && !(init as RequestInit)?.method).length;
    const before = lists();
    await waitFor(() => expect(screen.queryByText('123456')).toBeNull());
    await waitFor(() => expect(lists()).toBeGreaterThan(before));
  });
});
