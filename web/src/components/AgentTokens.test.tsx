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

  // Requirement 4 (Joplin GHSA-9728-v7ww-mxjv): no generic "an app wants access" prompt. Every
  // step that issues or raises a token names the agent and the access level it grants.
  it('names the agent and the access level on every issuing or raising step', async () => {
    stub([[/^POST \/api\/organizations\/org_a\/agent-tokens$/, { token: 'kyy_mcp_' + 'b'.repeat(64), agent_token: { ...token, name: 'ops-bot', scope: 'mcp:operate' }, disclosure: 'Shown once.' }, 201]]);
    const confirm = vi.fn((_message?: string) => true);
    vi.stubGlobal('confirm', confirm);
    render(<AgentTokens org="org_a" />);
    await screen.findByText('homelab');
    fireEvent.change(screen.getByLabelText('Agent name'), { target: { value: 'ops-bot' } });
    expect(screen.getByLabelText('Token to be created').textContent).toBe('Creates a token for "ops-bot" with Read only access: it can read endpoints, containers, logs, resource usage and pending updates, and change nothing.');
    fireEvent.click(screen.getByLabelText(/Operator: restart and apply updates/));
    expect(screen.getByLabelText('Token to be created').textContent).toMatch(/^Creates a token for "ops-bot" with Operator access: it can also restart/);
    fireEvent.click(screen.getByRole('button', { name: 'Create agent token' }));
    expect(confirm.mock.calls[0][0]).toMatch(/^Create a token for "ops-bot" with Operator access\?/);
    expect((await screen.findByRole('region', { name: 'New agent token' })).textContent).toMatch(/Token for "ops-bot" with Operator access: it can also restart/);
    fireEvent.change(screen.getByLabelText('Access for homelab'), { target: { value: 'mcp:operate' } });
    expect(confirm.mock.calls[1][0]).toMatch(/^Change "homelab" from Read only to Operator access\?/);
    for (const [message] of confirm.mock.calls) expect(message).not.toMatch(/an app|wants access|an application/i);
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
