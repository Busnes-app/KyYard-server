import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { StatusFeeds } from './StatusFeeds';

const feed = { id: 'feed_1', organization_id: 'org_a', name: 'uptime', created_by: 'usr_admin', created_at: '2026-10-31T10:00:00Z' };
const url = 'https://yard.example/api/feeds/kyy_feed_' + 'a'.repeat(64) + '/down.rss';

function stub(extra: Array<[RegExp, unknown, number?]> = []) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    for (const [re, body, status] of extra) if (re.test(key)) return new Response(status === 204 ? null : JSON.stringify(body), { status: status ?? 200 });
    if (key === 'GET /api/organizations/org_a/status-feeds') return new Response(JSON.stringify([feed]));
    throw new Error(key);
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

const sent = (fn: ReturnType<typeof stub>, method: string) => fn.mock.calls.filter(([, init]) => (init as RequestInit)?.method === method);

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('StatusFeeds', () => {
  it('creates a feed and shows its URL once', async () => {
    const fetchMock = stub([[/^POST \/api\/organizations\/org_a\/status-feeds$/, { url, status_feed: { ...feed, name: 'pager' }, disclosure: 'Shown once.' }, 201]]);
    render(<StatusFeeds org="org_a" />);
    expect(await screen.findByText('uptime')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Feed name'), { target: { value: 'pager' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create status feed' }));
    expect(await screen.findByText(url)).toBeTruthy();
    expect(JSON.parse(String((sent(fetchMock, 'POST')[0][1] as RequestInit).body))).toEqual({ name: 'pager' });
    fireEvent.click(screen.getByRole('button', { name: 'I have saved the URL' }));
    expect(screen.queryByText(url)).toBeNull();
  });

  it('shows the limit as fixed text', async () => {
    stub([[/^POST /, { error: 'x', code: 'status_feed_limit' }, 409]]);
    render(<StatusFeeds org="org_a" />);
    await screen.findByText('uptime');
    fireEvent.change(screen.getByLabelText('Feed name'), { target: { value: 'one more' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create status feed' }));
    expect(await screen.findByText(/has 20 status feeds/)).toBeTruthy();
  });

  it('revokes only after confirmation', async () => {
    const fetchMock = stub([[/^DELETE \/api\/organizations\/org_a\/status-feeds\/feed_1$/, null, 204]]);
    const confirm = vi.fn(() => false);
    vi.stubGlobal('confirm', confirm);
    render(<StatusFeeds org="org_a" />);
    await screen.findByText('uptime');
    fireEvent.click(screen.getByRole('button', { name: 'Revoke uptime' }));
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(/URL stops working immediately/));
    expect(sent(fetchMock, 'DELETE').length).toBe(0);
    confirm.mockReturnValue(true);
    fireEvent.click(screen.getByRole('button', { name: 'Revoke uptime' }));
    await waitFor(() => expect(sent(fetchMock, 'DELETE').length).toBe(1));
  });
});
