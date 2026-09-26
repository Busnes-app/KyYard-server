import { useState } from 'react';
import { secureFetch } from '../api';
import { offlineWrite, parseNamespaces, refusal, type Endpoint } from '../tenant';

type Manifest = { manifest: string; manifest_file: string; command: string; namespaces: string[]; note: string };

// downloadManifest saves text as file through a temporary object URL.
export function downloadManifest(file: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/yaml' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = file;
  link.click();
  URL.revokeObjectURL(url);
}

// ManifestRegeneration records the namespaces a cluster's agent may deploy to and shows the
// RBAC-only manifest that grants them, for a cluster-admin to apply. The server audits it.
export function ManifestRegeneration({ org, endpoint, onSaved }: { org: string; endpoint: Endpoint; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState((endpoint.deploy_namespaces ?? []).join(' '));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<Manifest | null>(null);
  const [copied, setCopied] = useState('');
  const copy = async (text: string) => {
    try { await navigator.clipboard.writeText(text); setCopied('Copied.'); } catch { setCopied('Copy failed; open the manifest and copy it by hand.'); }
  };
  const save = async () => {
    setBusy(true); setError('');
    try {
      const r = await secureFetch(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint.id)}/manifest`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ namespaces: parseNamespaces(text) }) });
      if (r.ok) { setResult(await r.json() as Manifest); onSaved(); return; }
      setError(await refusal(r, { forbidden: 'Only an administrator can change the namespaces a cluster deploys to.', invalid: 'List at most 32 namespaces by name: lower-case letters, digits and hyphens; not kyyard-agent or a kube- namespace.' }));
    } catch { setError(offlineWrite); } finally { setBusy(false); }
  };
  return <section className="dr-stack" aria-label="Deploy namespaces">
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close manifest' : 'Regenerate manifest'}</button>
    {open && <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <label>Namespaces to deploy to<input value={text} onChange={(e) => setText(e.target.value)} disabled={busy} autoComplete="off" placeholder="shop billing" /></label>
      <p>Saving replaces the list. KyYard maps applications only to listed namespaces, and the agent checks its own access in the cluster before every apply, so nothing is deployed until the manifest is applied.</p>
      <button disabled={busy}>Save and show manifest</button>
      {error && <p role="alert">{error}</p>}
    </form>}
    {result && <div className="dr-alert dr-alert-warn" role="region" aria-label="Namespace manifest">
      <pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{result.command}</pre>
      <details><summary>Manifest</summary><pre className="font-mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}>{result.manifest}</pre></details>
      <p>{result.note}</p>
      {copied && <p role="status">{copied}</p>}
      <button onClick={() => downloadManifest(result.manifest_file, result.manifest)}>Download manifest</button>{' '}
      <button className="btn-secondary" onClick={() => { void copy(result.manifest); }}>Copy manifest</button>{' '}
      <button className="btn-secondary" onClick={() => setResult(null)}>Dismiss</button>
    </div>}
  </section>;
}
