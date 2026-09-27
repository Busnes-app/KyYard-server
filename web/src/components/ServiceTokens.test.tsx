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
});
