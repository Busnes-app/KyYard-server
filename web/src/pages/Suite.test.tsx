import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { Suite } from './Suite';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

const status = {
  configured: true,
  runner: { release_set: '0.5.0', runner_id: 'r-1', plan_revision: 7, plan_owner: 'runner' },
  operation: null,
  targets: [{ name: 'nas', kind: 'docker' }, { name: 'k3s', kind: 'kubernetes' }],
  apps: [{ name: 'kynotes', category: 'ky', version: '1.4.0', target: 'nas', state: 'installed', health: { ok: true, checked: '2026-10-16T10:00:00Z' }, last_result: { step: 'kynotes.health', status: 'ok', time: '2026-10-16T10:00:00Z' } }],
};
const catalog = {
  release_set: '0.5.0',
  apps: [
    { name: 'kynotes', description: 'Notes', category: 'ky', version: '1.5.0', depends_on: [], installed: true, upgrade: { min_from: '1.0.0', no_skip: [], migrates_data: false } },
    { name: 'hello', description: 'Hello world', category: 'third-party', version: '1.11.0', depends_on: ['kynotes'], installed: false, upgrade: { min_from: '1.0.0', no_skip: [], migrates_data: false } },
  ],
};
const opID = '5a1e5b8e-4b7a-4c55-9a37-2f1c4f0e8b11';
const operation = (state: string, steps: unknown[] = [], error: unknown = null) => ({ id: opID, actor_id: 'usr_a', kind: 'install', apps: ['hello'], target: 'nas', state, started: '2026-10-16T10:00:00Z', finished: state === 'running' ? null : '2026-10-16T10:01:00Z', error_code: '', operation: { id: opID, kind: 'install', apps: ['hello'], actor: 'usr_a', state, started: '2026-10-16T10:00:00Z', finished: null, steps, error } });

type Post = { body: Record<string, unknown>; csrf: string | null };

function serve(opts: { configured?: boolean; start?: Response; reads?: unknown[] } = {}) {
  const posts: Post[] = [];
  const reads = [...(opts.reads ?? [])];
  let opReads = 0;
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === 'POST') {
      posts.push({ body: JSON.parse(String(init.body)), csrf: new Headers(init.headers).get('X-CSRF-Token') });
      return opts.start ?? json(operation('running'), 202);
    }
    if (url === '/api/suite/status') return json(opts.configured === false ? { configured: false } : status);
    if (url === '/api/suite/catalog') return json(catalog);
    if (url === `/api/suite/operations/${opID}`) { opReads++; return json(reads.length > 1 ? reads.shift() : reads[0]); }
    return json({ error: 'not found' }, 404);
  }));
  return { posts, opReads: () => opReads };
}

beforeEach(() => {
  document.cookie = 'ky_csrf=csrf-test';
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.open = true; } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = 'ky_csrf=; Max-Age=0'; Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal'); });

it('explains the hand-off and offers nothing when no runner is configured', async () => {
  serve({ configured: false });
  render(<Suite totp={false} />);
  expect(await screen.findByText('Not configured')).toBeTruthy();
  expect(screen.getByText(/hand-off/)).toBeTruthy();
  expect(screen.queryAllByRole('button')).toHaveLength(0);
});

it('lists installed apps with the newest version and the catalog apps not installed', async () => {
  serve();
  render(<Suite totp={false} />);
  const installed = within((await screen.findByText('Installed')).closest('section')!);
  expect(installed.getByText('kynotes')).toBeTruthy();
  expect(installed.getByText('1.4.0')).toBeTruthy();
  expect(await installed.findByText('1.5.0')).toBeTruthy();
  expect(installed.getByText('healthy')).toBeTruthy();
  expect(installed.getByRole('button', { name: 'Uninstall kynotes' })).toBeTruthy();
  expect(installed.getByRole('button', { name: 'Unmanage kynotes' })).toBeTruthy();
  const available = within(screen.getByText('Available').closest('section')!);
  expect(await available.findByText('Hello world')).toBeTruthy();
  expect(available.queryByText('Notes')).toBeNull();
  expect(screen.getByText('Plan revision 7 · owner runner · release set 0.5.0')).toBeTruthy();
});

it('requires the typed app name, then posts the install with password, code and CSRF and clears the secrets', async () => {
  const { posts } = serve({ reads: [operation('running')] });
  render(<Suite totp />);
  fireEvent.click(await screen.findByRole('button', { name: 'Install hello' }));
  const dialogSubmit = screen.getAllByRole('button', { name: 'Install hello' }).find((b) => b.getAttribute('type') === 'submit') as HTMLButtonElement;
  fireEvent.change(screen.getByLabelText('Target'), { target: { value: 'k3s' } });
  fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'pw-123' } });
  fireEvent.change(screen.getByLabelText('Authentication code'), { target: { value: '123456' } });
  fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: 'hell' } });
  expect(dialogSubmit.disabled).toBe(true);
  fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: 'hello' } });
  expect(dialogSubmit.disabled).toBe(false);
  fireEvent.click(dialogSubmit);
  await waitFor(() => expect(posts).toHaveLength(1));
  expect(posts[0]).toEqual({ body: { kind: 'install', apps: ['hello'], confirm: 'hello', password: 'pw-123', code: '123456', target: 'k3s' }, csrf: 'csrf-test' });
  expect(await screen.findByText('Operation')).toBeTruthy();
});

it('upgrades the suite with the explicit no-backup acknowledgement and no code field without MFA', async () => {
  const { posts } = serve({ reads: [operation('running')] });
  render(<Suite totp={false} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Upgrade suite' }));
  expect(screen.queryByLabelText('Authentication code')).toBeNull();
  const ack = screen.getByLabelText(/No backup acknowledgement/) as HTMLInputElement;
  expect(screen.getByText(/Upgrades that migrate data are refused without a verified pre-upgrade backup/)).toBeTruthy();
  fireEvent.click(ack);
  fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'pw-123' } });
  fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: 'suite' } });
  fireEvent.submit(screen.getByLabelText('Current password').closest('form')!);
  await waitFor(() => expect(posts).toHaveLength(1));
  expect(posts[0].body).toEqual({ kind: 'upgrade', apps: [], confirm: 'suite', password: 'pw-123', acknowledge_no_backup: true });
});

it('shows the runner refusal code and message and keeps the dialog open', async () => {
  serve({ start: json({ error: 'operation 1 is running', code: 'busy' }, 409) });
  render(<Suite totp={false} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Uninstall kynotes' }));
  fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'pw-123' } });
  fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: 'kynotes' } });
  fireEvent.submit(screen.getByLabelText('Current password').closest('form')!);
  expect(await screen.findByText('operation 1 is running (busy)')).toBeTruthy();
  expect((screen.getByLabelText('Current password') as HTMLInputElement).value).toBe('');
});

it('polls the operation until it ends and shows its steps and diagnostics', async () => {
  const done = operation('failed', [{ id: 'hello.deploy', status: 'failed', time: '2026-10-16T10:00:30Z', diagnostics: 'image pull refused' }], { code: 'step_failed', message: 'hello.deploy failed' });
  const server = serve({ reads: [operation('running'), done] });
  render(<Suite totp={false} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Install hello' }));
  fireEvent.change(screen.getByLabelText('Current password'), { target: { value: 'pw-123' } });
  fireEvent.change(screen.getByLabelText(/to confirm/), { target: { value: 'hello' } });
  fireEvent.submit(screen.getByLabelText('Current password').closest('form')!);
  expect(await screen.findByText(/Running; this panel refreshes/)).toBeTruthy();
  expect(await screen.findByText('image pull refused', {}, { timeout: 4000 })).toBeTruthy();
  expect(screen.getByText('hello.deploy failed (step_failed)')).toBeTruthy();
  expect(server.opReads()).toBe(2);
});
