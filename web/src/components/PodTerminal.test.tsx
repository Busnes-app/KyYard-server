import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { PodTerminal } from './PodTerminal';
import type { Pod } from '../tenant';

vi.mock('@xterm/xterm', () => ({ Terminal: class {
  options = { disableStdin: false };
  open() {} focus() {} resize() {} write() {} dispose() {}
  onData() { return { dispose() {} }; }
  onBinary() { return { dispose() {} }; }
} }));
class Socket {
  static OPEN = 1;
  static current: Socket;
  readonly readyState = 1;
  bufferedAmount = 0;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  close = vi.fn();
  constructor(readonly url: URL) { Socket.current = this; }
  send(data: string) { this.sent.push(data); }
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const container = (name: string) => ({ name, image: 'nginx:1', image_id: '', state: 'running', reason: '', ready: true, restart_count: 0 });
const pod: Pod = { uid: '7f3c-uid', namespace: 'shop', name: 'web-7c9', phase: 'Running', node: 'n1', owner_kind: 'Deployment', owner_name: 'web', started_at: '', containers: [container('web'), container('log')] };

it('asks container and shell, confirms the pod name and opens the pod exec route', () => {
  vi.stubGlobal('WebSocket', Socket);
  document.cookie = 'ky_csrf=pod-csrf';
  render(<PodTerminal base="/api/organizations/a/endpoints/ep_k" pod={pod} scope="Cluster prod" />);
  expect(screen.queryByLabelText('Container user')).toBeNull();
  fireEvent.change(screen.getByLabelText('Container'), { target: { value: 'log' } });
  fireEvent.change(screen.getByLabelText('Shell executable'), { target: { value: '  /bin/bash ' } });
  const open = screen.getByRole('button', { name: 'Open terminal in log' });
  expect(open.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Confirm pod name'), { target: { value: 'web-7c9' } });
  fireEvent.click(open);
  act(() => Socket.current.onopen?.());
  const socket = Socket.current;
  expect(socket.url.pathname).toBe('/api/organizations/a/endpoints/ep_k/pods/shop/web-7c9/exec');
  expect(socket.url.search).toBe('');
  expect(JSON.parse(socket.sent[0] ?? '')).toEqual({ csrf: 'pod-csrf', spec: { pod: { namespace: 'shop', name: 'web-7c9', container: 'log', uid: '7f3c-uid' }, argv: ['/bin/bash'] }, confirm: 'web-7c9', size: { rows: 24, columns: 80 } });
  expect(screen.getByLabelText('Pod terminal')).toBeTruthy();
});

it('refuses to open a pod whose identity the inventory does not report', () => {
  render(<PodTerminal base="/api/x" pod={{ ...pod, uid: undefined }} scope="Cluster prod" />);
  expect(screen.getByText(/does not report this pod's identity/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /Open terminal/ })).toBeNull();
});

it.each([
  ['forbidden', "The agent's role does not allow a terminal here. Regenerate and apply the cluster manifest, then retry."],
  ['pod_security', 'This pod or its namespace does not meet Pod Security baseline, so terminals are refused.'],
  ['<b>x</b>', 'Terminal ended or was refused. Process state may be unknown. Check access, target and agent version before opening another terminal.'],
])('words the %s close reason in fixed text', (reason, text) => {
  vi.stubGlobal('WebSocket', Socket);
  render(<PodTerminal base="/api/x" pod={pod} scope="Cluster prod" />);
  fireEvent.change(screen.getByLabelText('Confirm pod name'), { target: { value: 'web-7c9' } });
  fireEvent.click(screen.getByRole('button', { name: 'Open terminal in web' }));
  act(() => Socket.current.onopen?.());
  act(() => Socket.current.onmessage?.({ data: JSON.stringify({ v: 1, type: 'exec.close', payload: { stream: 's1', reason } }) }));
  expect(screen.getByRole('status').textContent).toBe(text);
});
