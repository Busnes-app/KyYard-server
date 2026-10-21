import { useState } from 'react';
import { tenantWrite, useTenantResource, type AgentToken, type AgentTokenIssue, type AgentScope } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';

const scopeText: Record<AgentScope, string> = { 'mcp:read': 'Read only', 'mcp:operate': 'Operator' };
const scopeGrants: Record<AgentScope, string> = { 'mcp:read': 'it can read endpoints, containers, logs, resource usage and pending updates, and change nothing', 'mcp:operate': 'it can also restart containers and workloads and apply pending image updates' };
const operatorWarning = 'The agent can restart containers and workloads and apply pending image updates. Anyone who can message the agent can try to make it do so, and so can text a container prints into its log.';

// The copy-ready agent configuration reads the token from the environment, so it is never
// pasted into a config file that may be committed or synced.
function agentConfig(origin: string) {
  const url = `${origin}/mcp`;
  return {
    openclaw: `"mcp": { "servers": { "kyyard": {\n  "url": "${url}",\n  "transport": "streamable-http",\n  "headers": { "Authorization": "Bearer \${KYYARD_TOKEN}" }\n}}}`,
    hermes: `mcp_servers:\n  kyyard:\n    url: "${url}"\n    headers:\n      Authorization: "Bearer \${KYYARD_TOKEN}"`,
    skill: `# Hermes Agent\nhermes skills install ${origin}/agent/skills/kyyard/SKILL.md --category devops\n\n# OpenClaw (any skills root works, for example <workspace>/skills)\nmkdir -p ~/.agents/skills/kyyard\ncurl -fsSo ~/.agents/skills/kyyard/SKILL.md ${origin}/agent/skills/kyyard/SKILL.md`,
  };
}

export function AgentTokens({ org }: { org: string }) {
  const base = `/api/organizations/${encodeURIComponent(org)}/agent-tokens`;
  const tokens = useTenantResource<AgentToken[]>(base);
  const [name, setName] = useState('');
  const [scope, setScope] = useState<AgentScope>('mcp:read');
  const [issued, setIssued] = useState<AgentTokenIssue | null>(null);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const create = async () => {
    if (scope === 'mcp:operate' && !window.confirm(`Create a token for "${name.trim()}" with Operator access? ${operatorWarning}`)) return;
    setBusy(true);
    setMessage('');
    try {
      const resp = await secureFetch(base, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: name.trim(), scope }) });
      if (resp.status === 409) { setMessage('This organization has 20 agent tokens. Revoke one before creating another.'); return; }
      if (!resp.ok) { setMessage('Could not create the agent token. Names are 1 to 64 characters.'); return; }
      setIssued(await resp.json());
      setName('');
      setScope('mcp:read');
      tokens.reload();
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  const change = async (t: AgentToken, next: AgentScope) => {
    if (next === t.scope) return;
    if (next === 'mcp:operate' && !window.confirm(`Change "${t.name}" from ${scopeText[t.scope]} to Operator access? ${operatorWarning}`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/${encodeURIComponent(t.id)}`, 'PATCH', { scope: next });
    setBusy(false);
    setMessage(err || `"${t.name}" is now ${scopeText[next].toLowerCase()}.`);
    tokens.reload();
  };
  const revoke = async (t: AgentToken) => {
    if (!window.confirm(`Revoke "${t.name}"? The agent loses access immediately.`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/${encodeURIComponent(t.id)}`, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) tokens.reload();
  };
  const config = agentConfig(window.location.origin);

  return (
    <section className="panel" aria-labelledby="agent-tokens-heading">
      <div className="panel-header">
        <h2 id="agent-tokens-heading" style={{ fontSize: 16 }}>AI agents (MCP)</h2>
      </div>
      <p>An agent token lets an AI agent such as OpenClaw or Hermes Agent use this organization over MCP at <code>{window.location.origin}/mcp</code>. <strong>Read only</strong> sees endpoints, containers, logs, resource usage and pending updates. <strong>Operator</strong> can also restart a container or workload and apply an update plan someone made in KyYard. No agent can stop, delete, run commands in or reconfigure anything. A token unused for 90 days is revoked; agent tokens never work as kyPulse service tokens, nor the reverse.</p>
      <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void create(); }}>
        <label>Agent name<input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} disabled={busy} autoComplete="off" placeholder="homelab assistant" /></label>
        <fieldset className="ky-choices">
          <legend>Access</legend>
          <label><input type="radio" name="agent-scope" checked={scope === 'mcp:read'} onChange={() => setScope('mcp:read')} disabled={busy} /> Read only (recommended)</label>
          <label><input type="radio" name="agent-scope" checked={scope === 'mcp:operate'} onChange={() => setScope('mcp:operate')} disabled={busy} /> Operator: restart and apply updates</label>
          {scope === 'mcp:operate' && <p className="dr-alert dr-alert-warn" role="note">{operatorWarning}</p>}
        </fieldset>
        {name.trim() !== '' && <p role="status" aria-label="Token to be created">Creates a token for "{name.trim()}" with {scopeText[scope]} access: {scopeGrants[scope]}.</p>}
        <button disabled={busy || name.trim() === ''}>Create agent token</button>
      </form>
      {issued && (
        <div className="dr-alert dr-alert-warn ky-enrollment" role="region" aria-label="New agent token">
          <p>Token for "{issued.agent_token.name}" with {scopeText[issued.agent_token.scope]} access: {scopeGrants[issued.agent_token.scope]}. {issued.disclosure}</p>
          <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{issued.token}</pre>
          <p>Set it as <code>KYYARD_TOKEN</code> in the agent's environment, then add the server.</p>
          <p>OpenClaw:</p>
          <pre className="font-mono">{config.openclaw}</pre>
          <p>Hermes Agent:</p>
          <pre className="font-mono">{config.hermes}</pre>
          <p>The KyYard skill teaches the agent the tools and when to ask you first:</p>
          <pre className="font-mono">{config.skill}</pre>
          <button className="btn-secondary" onClick={() => setIssued(null)}>I have saved the token</button>
        </div>
      )}
      {message && <p role="status">{message}</p>}
      <StateNotice state={tokens.state} onRetry={tokens.reload} />
      {tokens.state === 'ready' && tokens.data && (tokens.data.length === 0 ? <p>No agent tokens.</p> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Agent</th><th>Access</th><th>Last used</th><th>Revoked if unused by</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>
              {tokens.data.map((t) => (
                <tr key={t.id}>
                  <td data-label="Agent">{t.name}</td>
                  <td data-label="Access">
                    <select aria-label={`Access for ${t.name}`} value={t.scope} disabled={busy} onChange={(e) => void change(t, e.target.value as AgentScope)}>
                      <option value="mcp:read">{scopeText['mcp:read']}</option>
                      <option value="mcp:operate">{scopeText['mcp:operate']}</option>
                    </select>
                  </td>
                  <td data-label="Last used">{t.last_used_at ? `${new Date(t.last_used_at).toLocaleString()}${t.last_ip ? ` from ${t.last_ip}` : ''}` : 'never'}</td>
                  <td data-label="Revoked if unused by">{new Date(t.revoke_after).toLocaleDateString()}</td>
                  <td data-label="Actions"><button className="btn-secondary" disabled={busy} aria-label={`Revoke ${t.name}`} onClick={() => void revoke(t)}>Revoke</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </section>
  );
}
