import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ContainerTerminal } from './ContainerTerminal';
import type { Container } from '../tenant';

const terminal = vi.hoisted(() => ({ writes: [] as Uint8Array[], callbacks: [] as (() => void)[], input: (_data: string) => {}, resize: vi.fn(), dispose: vi.fn() }));
vi.mock('@xterm/xterm', () => ({ Terminal: class {
  options = { disableStdin: false };
  open() {} focus() {}
  onData(callback: (data: string) => void) { terminal.input = callback; return { dispose() {} }; }
  onBinary() { return { dispose() {} }; }
  resize = terminal.resize;
  write(data: Uint8Array, callback: () => void) { terminal.writes.push(data); terminal.callbacks.push(callback); }
  dispose = terminal.dispose;
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
  receive(type: string, payload: object) { this.onmessage?.({ data: JSON.stringify({ v: 1, type, payload }) }); }
}
const container: Container = { id: 'a'.repeat(64), name: 'web', image_id: `sha256:${'b'.repeat(64)}`, image: 'app', state: 'running', status: 'Up', ports: [], networks: [], labels: {}, created_at: '' };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.clearAllMocks(); terminal.writes.length = 0; terminal.callbacks.length = 0; });
function open() {
  vi.stubGlobal('WebSocket', Socket);
  document.cookie = 'ky_csrf=exec-csrf';
  const view = render(<ContainerTerminal base="/api/organizations/team/endpoints/host" container={container} scope="Team / Prod / Host" />);
  fireEvent.change(screen.getByLabelText('Container user'), { target: { value: '1000' } });
  expect(screen.getByRole('button', { name: 'Open terminal as 1000' }).hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Confirm container name'), { target: { value: 'web' } });
  fireEvent.click(screen.getByRole('button', { name: 'Open terminal as 1000' }));
  act(() => Socket.current.onopen?.());
  return { view, socket: Socket.current };
}
it('confirms the target and user, carries CSRF in the start frame, and closes without retry', () => {
  const { view, socket } = open();
  expect(socket.url.search).toBe('');
  expect(JSON.parse(socket.sent[0] ?? '')).toEqual({ csrf: 'exec-csrf', spec: { container: container.id, image_id: container.image_id, user: '1000', argv: ['/bin/sh'] }, confirm: 'web', size: { rows: 24, columns: 80 } });
  act(() => socket.receive('exec.ready', { stream: 'stream-1' }));
  act(() => terminal.input('echo ✓\r'));
  const input = JSON.parse(socket.sent[1] ?? '');
  expect(input.type).toBe('exec.input'); expect(input.payload.stream).toBe('stream-1');
  expect(new TextDecoder().decode(Uint8Array.from(atob(input.payload.data), (char) => char.charCodeAt(0)))).toBe('echo ✓\r');
  fireEvent.change(screen.getByLabelText('Rows'), { target: { value: '40' } });
  fireEvent.click(screen.getByRole('button', { name: 'Resize terminal' }));
  expect(JSON.parse(socket.sent[2] ?? '').payload.size).toEqual({ rows: 40, columns: 80 });
  act(() => socket.receive('exec.close', { stream: 'stream-1', exit_code: 7 }));
  expect(screen.getByRole('status').textContent).toBe('Process exited with code 7.');
  expect(socket.close).toHaveBeenCalledOnce();
  view.unmount(); expect(terminal.dispose).toHaveBeenCalledOnce();
});
it('preserves binary output and stops at the display queue bound', () => {
  const { socket } = open();
  act(() => socket.receive('exec.ready', { stream: 'stream-1' }));
  const data = btoa('x'.repeat(32768));
  act(() => { for (let i = 0; i < 8; i++) socket.receive('exec.output', { stream: 'stream-1', data }); });
  expect(terminal.writes.length).toBe(8);
  act(() => socket.receive('exec.output', { stream: 'stream-1', data }));
  expect(terminal.writes.length).toBe(8);
  expect(screen.getByRole('status').textContent).toContain('display buffer');
  expect(socket.close).toHaveBeenCalledOnce();
});
it('refuses another stream and releases the socket on unmount', () => {
  const { view, socket } = open();
  act(() => socket.receive('exec.ready', { stream: 'stream-1' }));
  act(() => socket.receive('exec.output', { stream: 'foreign', data: btoa('no') }));
  expect(terminal.writes).toEqual([]); expect(socket.close).toHaveBeenCalledOnce();
  view.unmount(); expect(socket.onmessage).toBeNull();
});
