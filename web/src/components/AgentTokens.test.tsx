import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { AgentTokens } from './AgentTokens';

const token = { id: 'agt_1', organization_id: 'org_a', name: 'homelab', created_by: 'usr_admin', created_at: '2026-10-20T10:00:00Z', scope: 'mcp:read', revoke_after: '2027-01-18T10:00:00Z' };

function stub(extra: Array<[RegExp, unknown, number?]> = []) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    for (const [re, body, status] of extra) if (re.test(key)) return new Response(status === 204 ? null : JSON.stringify(body), { status: status ?? 200 });
    if (key === 'GET /api/organizations/org_a/agent-tokens') return new Response(JSON.stringify([token]));
    throw new Error(key);
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

const sent = (fn: ReturnType<typeof stub>, method: string) => fn.mock.calls.filter(([, init]) => (init as RequestInit)?.method === method);

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('AgentTokens', () => {
  it('creates a read-only token by default and shows it once with env-based config', async () => {
    const fetchMock = stub([[/^POST \/api\/organizations\/org_a\/agent-tokens$/, { token: 'kyy_mcp_' + 'a'.repeat(64), agent_token: { ...token, name: 'assistant' }, disclosure: 'Shown once.' }, 201]]);
    render(<AgentTokens org="org_a" />);
    expect(await screen.findByText('homelab')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Agent name'), { target: { value: 'assistant' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create agent token' }));
    expect(await screen.findByText('kyy_mcp_' + 'a'.repeat(64))).toBeTruthy();
    expect(JSON.parse(String((sent(fetchMock, 'POST')[0][1] as RequestInit).body))).toEqual({ name: 'assistant', scope: 'mcp:read' });
    expect(screen.getAllByText(/\$\{KYYARD_TOKEN\}/).length).toBe(2);
    expect(screen.getByText(/agent\/skills\/kyyard\/SKILL\.md/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'I have saved the token' }));
    expect(screen.queryByText('kyy_mcp_' + 'a'.repeat(64))).toBeNull();
  });

  it('warns and confirms before operator access, and sends nothing when declined', async () => {
    const fetchMock = stub();
    const confirm = vi.fn(() => false);
    vi.stubGlobal('confirm', confirm);
    render(<AgentTokens org="org_a" />);
    await screen.findByText('homelab');
    fireEvent.change(screen.getByLabelText('Agent name'), { target: { value: 'ops' } });
    fireEvent.click(screen.getByLabelText(/Operator: restart and apply updates/));
    expect(screen.getByRole('note').textContent).toMatch(/Anyone who can message the agent/);
    fireEvent.click(screen.getByRole('button', { name: 'Create agent token' }));
    fireEvent.change(screen.getByLabelText('Access for homelab'), { target: { value: 'mcp:operate' } });
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(sent(fetchMock, 'POST').length + sent(fetchMock, 'PATCH').length).toBe(0);
  });

  it('changes access and revokes after confirmation', async () => {
    const fetchMock = stub([[/^PATCH \/api\/organizations\/org_a\/agent-tokens\/agt_1$/, null, 204], [/^DELETE \/api\/organizations\/org_a\/agent-tokens\/agt_1$/, null, 204]]);
    vi.stubGlobal('confirm', vi.fn(() => true));
    render(<AgentTokens org="org_a" />);
    await screen.findByText('homelab');
    fireEvent.change(screen.getByLabelText('Access for homelab'), { target: { value: 'mcp:operate' } });
    await waitFor(() => expect(sent(fetchMock, 'PATCH').length).toBe(1));
    expect(JSON.parse(String((sent(fetchMock, 'PATCH')[0][1] as RequestInit).body))).toEqual({ scope: 'mcp:operate' });
    fireEvent.click(screen.getByRole('button', { name: 'Revoke homelab' }));
    await waitFor(() => expect(sent(fetchMock, 'DELETE').length).toBe(1));
  });
});
