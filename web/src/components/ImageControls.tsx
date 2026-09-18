import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';

interface Command { id: string; action: string; outcome: string; detail?: string }
function readCommand(value: unknown): Command {
  if (typeof value !== 'object' || value === null || !('id' in value) || typeof value.id !== 'string' ||
      !('action' in value) || typeof value.action !== 'string' || !('outcome' in value) || typeof value.outcome !== 'string') throw new Error('Invalid command response');
  return { id: value.id, action: value.action, outcome: value.outcome, detail: 'detail' in value && typeof value.detail === 'string' ? value.detail : undefined };
}
function object(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error('Invalid inventory');
  return Object.fromEntries(Object.entries(value));
}

// Removal always addresses an immutable image ID: refreshed tags must never retarget a decision.
function removalPreview(value: unknown, id: string): string {
  const inv = object(value); const snapshot = object(inv.snapshot);
  if (typeof inv.received_at !== 'string' || !Number.isFinite(Date.parse(inv.received_at)) || Date.now() - Date.parse(inv.received_at) > 180000) throw new Error('Inventory is stale. Wait for a fresh agent report before removing an image.');
  if (!Array.isArray(snapshot.images) || !Array.isArray(snapshot.containers)) throw new Error('Image preview unavailable.');
  if (Array.isArray(snapshot.truncated) && snapshot.truncated.includes('containers')) throw new Error('Container inventory is incomplete; removal preview cannot establish dependencies.');
  const image = snapshot.images.map(object).find((item) => item.id === id);
  if (!image) throw new Error('Image is no longer in inventory. Refresh before trying again.');
  const containers = snapshot.containers.map(object);
  if (containers.some((c) => typeof c.image_id !== 'string')) throw new Error('Container image identities are unavailable.');
  const dependents = containers.filter((c) => c.image_id === id);
  if (dependents.length) throw new Error(`Image is used by ${dependents.length} container(s), including stopped containers. Remove those containers first.`);
  const tags = Array.isArray(image.tags) ? image.tags.filter((tag): tag is string => typeof tag === 'string') : [];
  return `Image ID: ${id}\nTags: ${tags.join(', ') || '(untagged)'}\nInventory received: ${new Date(inv.received_at).toLocaleString()}\nNo dependent containers in this report. Docker checks dependencies again.\n\nRemove this image from this host, including its tags. Docker may refuse images with multiple tags. No force removal; containers and volumes are not removed.\nType the full image ID to confirm:`;
}

type Props = { base: string; active: boolean; scope: string; onActivity: () => void } & ({ kind: 'pull' } | { kind: 'remove'; imageID: string });
export function ImageControls(props: Props) {
  const { base, active, scope, onActivity } = props;
  const [reference, setReference] = useState('');
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [message, setMessage] = useState('');
  const [command, setCommand] = useState<Command | null>(null);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  useEffect(() => {
    if (!command || command.outcome) return;
    const abort = new AbortController(); let timer = 0;
    const poll = async () => {
      try {
        const r = await fetch(`${base}/commands/${encodeURIComponent(command.id)}`, { signal: abort.signal });
        if (!r.ok) throw new Error('Command status unavailable. Check recent activity before trying again.');
        const result = readCommand(await r.json());
        if (abort.signal.aborted) return;
        setCommand(result);
        if (result.outcome) {
          setMessage(`${result.action}: ${result.outcome}${result.detail ? ` — ${result.detail}` : ''}. Refresh inventory after the next agent report.`);
          setUncertain(result.outcome === 'unknown'); onActivity();
        } else timer = window.setTimeout(() => void poll(), 1500);
      } catch { if (!abort.signal.aborted) { setUncertain(true); setMessage('Command status unavailable. Check recent activity, then refresh inventory. Do not blindly retry.'); } }
    };
    timer = window.setTimeout(() => void poll(), 1500);
    return () => { abort.abort(); window.clearTimeout(timer); };
  }, [base, command?.id, command?.outcome, onActivity]);
  const act = async () => {
    if (busy || uncertain || !active || (command && !command.outcome)) return;
    const target = props.kind === 'pull' ? reference.trim() : props.imageID;
    if (props.kind === 'pull' && (!target || target.endsWith(':') || target.lastIndexOf(':') <= target.lastIndexOf('/') || target.startsWith('sha256:'))) {
      setMessage('Enter an image with an explicit tag or digest, such as nginx:1.28 or registry.example/app@sha256:…'); return;
    }
    setBusy(true); setMessage('');
    const abort = new AbortController(); request.current = abort;
    let sent = false;
    try {
      if (props.kind === 'remove') {
        const r = await fetch(`${base}/inventory`, { signal: abort.signal });
        if (!r.ok) throw new Error(r.status === 403 ? 'You do not have permission to inspect this image.' : 'Cannot load removal preview. Refresh inventory.');
        const preview = removalPreview(await r.json(), target);
        if (abort.signal.aborted || window.prompt(`${scope}\n\n${preview}`) !== target) return;
      } else if (!window.confirm(`Pull ${target}?\n${scope}\n\nDownloads the image to this host. Existing containers are not recreated. Private-registry credentials are not supported by this action.`)) return;
      if (abort.signal.aborted) return;
      sent = true;
      const r = await secureFetch(`${base}/commands`, { method: 'POST', signal: abort.signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: `image.${props.kind}`, reference: target, ...(props.kind === 'remove' ? { confirm: target } : {}) }) });
      if (abort.signal.aborted) return;
      if (!r.ok) {
        // A server failure can happen after intent was recorded; never silently retry a write.
        if (r.status >= 500) { setUncertain(true); setMessage('Server failed while submitting. Check recent activity before trying again.'); }
        else setMessage(r.status === 403 ? 'You do not have permission for this action.' : r.status === 409 ? 'Endpoint or image changed. Check recent activity and refresh inventory.' : r.status === 404 ? 'Image or endpoint is unavailable. Refresh inventory.' : `Image action refused (${r.status}). Check the reference and refresh inventory.`);
        onActivity(); return;
      }
      const result = readCommand(await r.json());
      if (abort.signal.aborted) return;
      setCommand(result); setUncertain(result.outcome === 'unknown');
      setMessage(result.outcome ? `${result.action}: ${result.outcome}${result.detail ? ` — ${result.detail}` : ''}. Refresh inventory after the next agent report.` : 'Command sent; waiting for the endpoint. Do not retry while its outcome is unknown.');
      onActivity();
    } catch (error: unknown) {
      if (!abort.signal.aborted) {
        setUncertain(sent);
        setMessage(sent ? 'Connection lost. The action may have run. Check recent activity, then refresh inventory. Do not blindly retry.' : error instanceof Error ? error.message : 'Cannot load image preview.');
        if (sent) onActivity();
      }
    } finally { if (!abort.signal.aborted) setBusy(false); }
  };
  const disabled = busy || uncertain || !active || (command !== null && !command.outcome);
  return <div>
    {props.kind === 'pull' ? <form className="ky-toolbar" onSubmit={(e) => { e.preventDefault(); void act(); }}>
      <label>Image reference <input value={reference} onChange={(e) => setReference(e.target.value)} placeholder="nginx:1.28" maxLength={512} required disabled={disabled} /></label>
      <button disabled={disabled}>Pull image</button>
    </form> : <button className="btn-secondary" disabled={disabled} onClick={() => void act()}>Remove image</button>}
    {message && <p role="status">{message}</p>}
  </div>;
}
