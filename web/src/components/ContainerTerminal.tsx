import { useEffect, useRef, useState } from 'react';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { cookieValue } from '../api';
import type { Container } from '../tenant';

type Start = { user: string; executable: string; confirm: string };
const maxChunk = 32 * 1024;
const maxQueued = 256 * 1024;

export function ContainerTerminal({ base, container, scope }: { base: string; container: Container; scope: string }) {
  const [user, setUser] = useState('');
  const [executable, setExecutable] = useState('/bin/sh');
  const [confirm, setConfirm] = useState('');
  const [start, setStart] = useState<Start | null>(null);
  return <section aria-label={`Terminal for ${container.name}`} style={{ marginTop: 12, minWidth: 300 }}>
    <h3>Terminal · {container.name}</h3>
    <p>{scope}</p><p className="font-mono" style={{ overflowWrap: 'anywhere' }}>Container {container.id}</p>
    <p>Organization administrators only. Choose the container user explicitly. Closing disconnects the terminal; it does not guarantee that the process stops. Sessions end after 15 minutes without input or 8 hours total.</p>
    {start ? <LiveTerminal key={container.id} base={base} container={container} start={start} /> : <form onSubmit={(event) => { event.preventDefault(); setStart({ user, executable, confirm }); }}>
      <label>Container user <input required maxLength={128} value={user} placeholder="e.g. 1000 or app" onChange={(e) => setUser(e.target.value)} /></label>
      <label>Shell executable <input required maxLength={1024} value={executable} onChange={(e) => setExecutable(e.target.value)} /></label>
      <label>Confirm container name <input required value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" /></label>
      <button className="btn-primary" disabled={confirm !== container.name || !user.trim() || !executable.trim()}>Open terminal as {user || 'chosen user'}</button>
    </form>}
  </section>;
}

function LiveTerminal({ base, container, start }: { base: string; container: Container; start: Start }) {
  const host = useRef<HTMLDivElement>(null);
  const resize = useRef<(columns: number, rows: number) => void>(() => {});
  const disconnect = useRef<() => void>(() => {});
  const [notice, setNotice] = useState('Connecting…');
  const [rows, setRows] = useState(24);
  const [columns, setColumns] = useState(80);
  useEffect(() => {
    if (!host.current) return;
    // Terminal output stays inside the emulator: no HTML, titles, links, clipboard
    // or downloaded content is derived from a container's escape sequences.
    const terminal = new Terminal({ rows: 24, cols: 80, scrollback: 1000, screenReaderMode: true, minimumContrastRatio: 4.5, linkHandler: { activate: () => {} } });
    terminal.open(host.current);
    const url = new URL(`${base}/containers/${encodeURIComponent(container.id)}/exec`, location.href);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(url);
    let stream = '';
    let live = true;
    let ended = false;
    let queued = 0;
    const stop = (message: string) => {
      if (ended) return;
      ended = true;
      terminal.options.disableStdin = true;
      if (live) setNotice(message);
      socket.close();
    };
    const send = (type: string, payload: object) => {
      if (ended || !stream || socket.readyState !== WebSocket.OPEN) return;
      if (socket.bufferedAmount > maxQueued) { stop('Disconnected: input could not keep up. Process state is unknown.'); return; }
      socket.send(JSON.stringify({ v: 1, type, payload: { ...payload, stream } }));
    };
    const input = (data: Uint8Array) => {
      for (let offset = 0; offset < data.length; offset += maxChunk) {
        const chunk = data.subarray(offset, offset + maxChunk);
        send('exec.input', { data: btoa(String.fromCharCode(...chunk)) });
      }
    };
    const dataSub = terminal.onData((data) => input(new TextEncoder().encode(data)));
    const binarySub = terminal.onBinary((data) => input(Uint8Array.from(data, (char) => char.charCodeAt(0))));
    disconnect.current = () => stop('Disconnected. Process state may be unknown.');
    resize.current = (cols, lines) => {
      if (!Number.isInteger(cols) || !Number.isInteger(lines) || cols < 1 || cols > 512 || lines < 1 || lines > 512) return;
      terminal.resize(cols, lines);
      send('exec.resize', { size: { rows: lines, columns: cols } });
    };
    socket.onopen = () => {
      if (ended) { socket.close(); return; }
      socket.send(JSON.stringify({ csrf: cookieValue('ky_csrf'), spec: { container: container.id, image_id: container.image_id, user: start.user, argv: [start.executable] }, confirm: start.confirm, size: { rows: 24, columns: 80 } }));
    };
    socket.onmessage = (event: MessageEvent<unknown>) => {
      try {
        if (typeof event.data !== 'string' || event.data.length > 65536) throw new Error();
        const frame: unknown = JSON.parse(event.data);
        if (!frame || typeof frame !== 'object' || !('v' in frame) || frame.v !== 1 || !('type' in frame) || !('payload' in frame)) throw new Error();
        const payload = frame.payload;
        if (!payload || typeof payload !== 'object' || !('stream' in payload) || typeof payload.stream !== 'string') throw new Error();
        if (stream && stream !== payload.stream) throw new Error();
        if (frame.type === 'exec.ready') {
          stream = payload.stream;
          if (live) setNotice('Connected. Terminal contents are not recorded.');
          terminal.focus();
        } else if (frame.type === 'exec.output') {
          if (!stream || !('data' in payload) || typeof payload.data !== 'string') throw new Error();
          const chunk = Uint8Array.from(atob(payload.data), (char) => char.charCodeAt(0));
          if (chunk.length > maxChunk || queued + chunk.length > maxQueued) { stop('Disconnected: terminal output exceeded the display buffer. Process state is unknown.'); return; }
          queued += chunk.length;
          terminal.write(chunk, () => { queued -= chunk.length; });
        } else if (frame.type === 'exec.close') {
          const code = 'exit_code' in payload ? payload.exit_code : undefined;
          stop(typeof code === 'number' && Number.isInteger(code) ? `Process exited with code ${code}.` : 'Terminal ended or was refused. Process state may be unknown. Check access, target and agent version before opening another terminal.');
        } else throw new Error();
      } catch { stop('Disconnected: invalid terminal response. Process state is unknown.'); }
    };
    socket.onerror = () => stop('Terminal connection failed. Check permissions and the configured site address.');
    socket.onclose = () => stop('Disconnected. Access may have changed; process state is unknown.');
    return () => {
      live = false; ended = true;
      socket.onmessage = null; socket.onopen = null; socket.onerror = null; socket.onclose = null;
      socket.close(); dataSub.dispose(); binarySub.dispose(); terminal.dispose();
      resize.current = () => {}; disconnect.current = () => {};
    };
  }, [base, container.id, container.image_id, start]);
  return <>
    <p role="status">{notice}</p>
    <div className="ky-toolbar">
      <label>Columns <input type="number" min={1} max={512} value={columns} onChange={(e) => setColumns(e.target.valueAsNumber)} /></label>
      <label>Rows <input type="number" min={1} max={512} value={rows} onChange={(e) => setRows(e.target.valueAsNumber)} /></label>
      <button className="btn-secondary" onClick={() => resize.current(columns, rows)}>Resize terminal</button>
      <button className="btn-secondary" onClick={() => disconnect.current()}>Disconnect</button>
    </div>
    <div ref={host} aria-label="Container terminal" style={{ overflow: 'auto', maxWidth: '80vw', padding: 8, background: '#000' }} />
  </>;
}
