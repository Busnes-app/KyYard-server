import { lazy, Suspense, useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
const ContainerTerminal = lazy(() => import('./ContainerTerminal').then((module) => ({ default: module.ContainerTerminal })));
import type { Container } from '../tenant';

interface Command { id: string; action: string; outcome: string; detail?: string }
export function ContainerControls({ base, container, active, scope, onRefresh }: { base: string; container: Container; active: boolean; scope: string; onRefresh: () => void }) {
  const actions = useRef<HTMLDetailsElement>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [command, setCommand] = useState<Command | null>(null);
  const [showLogs, setShowLogs] = useState(false);
  const [showTerminal, setShowTerminal] = useState(false);
  const logDialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { if (showLogs) logDialog.current?.showModal(); }, [showLogs]);
  const terminalDialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { if (showTerminal) terminalDialog.current?.showModal(); }, [showTerminal]);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    if (!command || command.outcome) return;
    const abort = new AbortController();
    const timer = window.setInterval(() => {
      fetch(`${base}/commands/${encodeURIComponent(command.id)}`, { signal: abort.signal }).then(async (r) => {
        if (!r.ok) throw new Error('Could not read command result. Check recent activity before trying again.');
        const data: Command = await r.json();
        if (abort.signal.aborted) return;
        setCommand(data);
        if (data.outcome) { setMessage(`${data.action}: ${data.outcome}${data.detail ? ` — ${data.detail}` : ''}`); onRefresh(); }
      }).catch((e: unknown) => { if (!abort.signal.aborted) { setMessage(e instanceof Error ? e.message : 'Command result unavailable.'); window.clearInterval(timer); } });
    }, 1500);
    return () => { window.clearInterval(timer); abort.abort(); };
  }, [base, command?.id, command?.outcome, onRefresh]);
  const act = async (action: 'start' | 'stop' | 'restart' | 'remove') => {
    setBusy(true); setMessage('');
    let confirm = '';
    try {
      if (action === 'remove') {
        const response = await fetch(`${base}/containers/${encodeURIComponent(container.id)}/removal`);
        if (!response.ok) { setMessage('Cannot load removal preview. Refresh the inventory.'); return; }
        const preview: { name: string; confirm_with: string; consequences: string[]; received_at: string } = await response.json();
        const typed = window.prompt(`Remove ${preview.name}?\n${scope}\nInventory received ${new Date(preview.received_at).toLocaleString()}\n\n${preview.consequences.join('\n')}\n\nType the full container name to confirm:`);
        if (typed !== preview.confirm_with) return;
        confirm = typed;
      } else if (!window.confirm(`${action} ${container.name}?\n${scope}\nContainer ID: ${container.id}`)) return;
      const response = await secureFetch(`${base}/commands`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: `container.${action}`, container: container.id, confirm, expects: { state: container.state, image_digest: container.image_id } }) });
      if (!alive.current) return;
      if (!response.ok) { setMessage(response.status === 403 ? 'You do not have permission for this action.' : response.status === 409 ? 'The endpoint or container changed. Refresh before trying again.' : `Action refused (${response.status}).`); return; }
      const result: Command = await response.json();
      if (!alive.current) return;
      setCommand(result); setMessage(result.outcome ? `${result.action}: ${result.outcome}` : 'Command sent; waiting for the endpoint. Do not retry while its outcome is unknown.');
    } catch { if (alive.current) setMessage('Connection lost. The action may have run. Check recent activity before trying again.'); }
    finally { if (alive.current) setBusy(false); }
  };
  const disabled = busy || !active || (command !== null && !command.outcome);
  return <>
    <details ref={actions} className="ky-container-actions" onKeyDown={(event) => {
      if (event.key === 'Escape' && actions.current?.open) { actions.current.open = false; actions.current.querySelector('summary')?.focus(); }
    }}>
      <summary aria-label={`Actions for ${container.name}`}>Actions</summary>
      <div className="ky-container-action-list" onClick={(event) => {
        if (event.target instanceof HTMLButtonElement && !event.target.disabled && actions.current) {
          actions.current.open = false; actions.current.querySelector('summary')?.focus();
        }
      }}>
      {container.state !== 'running' && <button className="btn-secondary" disabled={disabled} onClick={() => void act('start')}>Start</button>}
      {container.state === 'running' && <><button className="btn-secondary" disabled={disabled} onClick={() => void act('stop')}>Stop</button><button className="btn-secondary" disabled={disabled} onClick={() => void act('restart')}>Restart</button></>}
      <button className="btn-secondary" disabled={!active || container.state !== 'running'} onClick={() => setShowTerminal(!showTerminal)}>{showTerminal ? 'Close terminal' : 'Terminal'}</button>
      <button className="btn-secondary" disabled={!active} onClick={() => setShowLogs(!showLogs)}>{showLogs ? 'Close logs' : 'Logs'}</button>
      <button className="btn-secondary" disabled={disabled || ['running', 'paused', 'restarting'].includes(container.state)} onClick={() => void act('remove')}>Remove</button>
      </div>
    </details>
    {message && <p role="status">{message}</p>}
    {showTerminal && <dialog ref={terminalDialog} className="modal-window" aria-label={`Terminal for ${container.name}`} onCancel={(event) => { event.preventDefault(); setShowTerminal(false); }} onClose={() => setShowTerminal(false)} style={{ color: 'var(--ink-strong)', width: 'min(960px, calc(100vw - 32px))', maxWidth: 'none', maxHeight: 'calc(100dvh - 32px)', margin: 'auto' }}>
      <button className="btn-secondary" onClick={() => setShowTerminal(false)}>Close terminal</button>
      <Suspense fallback={<p role="status">Loading terminal…</p>}><ContainerTerminal key={`${base}/${container.id}/${container.image_id}`} base={base} container={container} scope={scope} /></Suspense>
    </dialog>}
    {showLogs && <dialog ref={logDialog} className="modal-window ky-log-dialog" aria-label={`Logs for ${container.name}`} onCancel={(event) => { event.preventDefault(); setShowLogs(false); }} onClose={() => setShowLogs(false)}>
      <button className="btn-secondary" onClick={() => setShowLogs(false)}>Close logs</button>
      <ContainerLogs key={container.id} url={`${base}/containers/${encodeURIComponent(container.id)}/logs`} name={container.name} />
    </dialog>}
  </>;
}

export function ContainerLogs({ url, name }: { url: string; name: string }) {
  const [text, setText] = useState('');
  const [notice, setNotice] = useState('');
  const [search, setSearch] = useState('');
  const [timestamps, setTimestamps] = useState(true);
  const [following, setFollowing] = useState(false);
  const [busy, setBusy] = useState(false);
  const source = useRef<EventSource | null>(null);
  const request = useRef<AbortController | null>(null);
  const params = new URLSearchParams({ tail: '200', timestamps: timestamps ? '1' : '0', search });
  const stop = () => { source.current?.close(); source.current = null; request.current?.abort(); request.current = null; setFollowing(false); setBusy(false); };
  useEffect(() => () => { source.current?.close(); request.current?.abort(); }, []);
  const load = async () => {
    stop(); setNotice(''); setBusy(true);
    const abort = new AbortController(); request.current = abort;
    try {
      const response = await fetch(`${url}?${params}`, { signal: abort.signal });
      if (!response.ok) { setNotice(response.status === 403 ? 'You do not have permission to read logs.' : `Logs unavailable (${response.status}).`); return; }
      const body = await response.text();
      if (abort.signal.aborted) return;
      setText(body.slice(-262144));
      if (body.length > 262144) setNotice('Showing the last 256 KiB in this browser. Download a bounded log window for more.');
    } catch { if (!abort.signal.aborted) setNotice('Log request interrupted.'); }
    finally { if (!abort.signal.aborted) setBusy(false); }
  };
  const follow = () => {
    stop(); setText(''); setNotice(''); setFollowing(true);
    const stream = new EventSource(`${url}?${params}&follow=1`); source.current = stream;
    stream.onmessage = (e) => { setText((old) => (old + e.data + '\n').slice(-262144)); };
    stream.addEventListener('notice', (e) => { if (e instanceof MessageEvent) setNotice(e.data); });
    // EventSource normally reconnects automatically; a closed/revoked log requires a new
    // deliberate request, so close here instead of silently replaying old output.
    stream.onerror = () => { stream.close(); source.current = null; setFollowing(false); setNotice((old) => old || 'Stream ended or unavailable. Start a new request to reconnect.'); };
  };
  return <section aria-label={`Logs for ${name}`} style={{ marginTop: 12 }}>
    <h3>Logs · {name}</h3><div className="ky-toolbar">
      <input aria-label="Filter logs" placeholder="Search log text" value={search} onChange={(e) => { stop(); setSearch(e.target.value); }} />
      <label><input style={{ width: 'auto' }} type="checkbox" checked={timestamps} onChange={(e) => { stop(); setTimestamps(e.target.checked); }} /> Timestamps</label>
      <button className="btn-secondary" disabled={busy} onClick={() => void load()}>Load logs</button>
      {following ? <button onClick={stop}>Stop following</button> : <button disabled={busy} onClick={follow}>Follow</button>}
      <a href={`${url}?${params}&download=1`}>Download</a>
    </div><p>Last 200 lines; browser display keeps the last 256 KiB. Server limits and gaps appear below.</p>
    {notice && <p role="status">KyYard: {notice}</p>}
    <pre className="ky-log" tabIndex={0}>{text || 'No output loaded.'}</pre>
  </section>;
}
