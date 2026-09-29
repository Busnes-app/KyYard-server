import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { CommandResult, ContainerConfigurationForm } from './ContainerConfigurationForm';
import type { DirectCommand } from '../tenant';
import type { Container, ContainerConfiguration } from '../tenant';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

const id = 'a'.repeat(64);
const imageID = `sha256:${'b'.repeat(64)}`;
const base = '/api/organizations/a/endpoints/ep_1';
const container: Container = { id, name: 'web', image: 'nginx:1', image_id: imageID, state: 'running', status: 'Up', created_at: '1970-01-01T00:00:07Z', ports: [], labels: {}, networks: [] };
const config = (over: Partial<ContainerConfiguration> = {}): ContainerConfiguration => ({
  target: { container_id: id, image_id: imageID, created_unix: 7 }, observed_at: '2026-09-29T12:00:00Z', name: 'web',
  image: { reference: 'nginx:1', digest: `sha256:${'d'.repeat(64)}` }, image_id: imageID,
  command: ['nginx', '-g', 'daemon off;'], entrypoint: [], user: '', working_dir: '', hostname: '',
  env: [{ name: 'SECRET', value: 'hunter2' }, { name: 'MODE', value: 'prod' }], labels: { b: '2', a: '1' },
  restart: 'on-failure', restart_retries: 3, ports: [{ host: 8080, container: 80, protocol: 'tcp' }],
  mounts: [{ kind: 'bind', source: '/srv/data', target: '/data', read_only: false }], network_mode: 'bridge', networks: [],
  resources: { nano_cpus: 1_500_000_000, memory_bytes: 268435456, memory_swap_bytes: -1, pids_limit: 0 },
  healthcheck: { test: ['NONE'], interval_seconds: 0, timeout_seconds: 0, start_period_seconds: 0, retries: 0 },
  privileged: false, read_only_rootfs: false, init: false, tty: false, stdin_open: false,
  cap_add: ['CAP_NET_ADMIN'], cap_drop: [], security_opt: [], extra_hosts: [], dns: [], devices: [],
  log: { driver: 'json-file', options: { 'max-size': '10m', 'max-file': '3' } }, stop_signal: '', unsupported: [], ...over,
});
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const edit = (over: Partial<ContainerConfiguration> = {}, props: object = {}) => render(<ContainerConfigurationForm base={base} mode="edit" initial={config(over)} container={container} onSent={() => {}} {...props} />);
const save = () => screen.getByRole('button', { name: 'Save and recreate' }) as HTMLButtonElement;
const confirmName = (name: string) => fireEvent.change(screen.getByLabelText(/^Type the container name/), { target: { value: name } });

it('round-trips a configuration with no changes and keeps Save disabled', () => {
  edit();
  expect(screen.getByText('No changes.')).toBeTruthy();
  confirmName('web');
  expect(save().disabled).toBe(true);
  expect(screen.getByText(/Health check disabled/)).toBeTruthy();
  expect((screen.getByLabelText('CPUs') as HTMLInputElement).value).toBe('1.5');
  expect((screen.getByLabelText('Memory (MiB)') as HTMLInputElement).value).toBe('256');
  expect((screen.getByLabelText('Capabilities to add, one per line') as HTMLTextAreaElement).value).toBe('CAP_NET_ADMIN');
});

it('keeps environment values out of the DOM until revealed', () => {
  const { container: root } = edit();
  expect(root.innerHTML).not.toContain('hunter2');
  expect(screen.queryByLabelText('Value of SECRET')).toBeNull();
  expect(screen.getByRole('button', { name: 'Reveal value of SECRET' }).textContent).toBe('••••••');
  fireEvent.click(screen.getByRole('button', { name: 'Reveal value of SECRET' }));
  const value = screen.getByLabelText('Value of SECRET') as HTMLTextAreaElement;
  expect(value.tagName).toBe('TEXTAREA');
  expect(root.innerHTML).toContain('hunter2');
  expect(value.getAttribute('autocomplete')).toBe('off');
  expect(value.hasAttribute('data-1p-ignore')).toBe(true);
  expect(value.getAttribute('data-lpignore')).toBe('true');
  expect(value.hasAttribute('data-bwignore')).toBe(true);
  expect(root.innerHTML).not.toContain('prod');
  fireEvent.change(value, { target: { value: 'changed' } });
  expect(screen.getByText('Changes: env')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Hide value of SECRET' }));
  expect(root.innerHTML).not.toContain('changed');
  fireEvent.click(screen.getByRole('button', { name: 'Reveal all' }));
  expect((screen.getByLabelText('Value of SECRET') as HTMLTextAreaElement).value).toBe('changed');
  expect((screen.getByLabelText('Value of MODE') as HTMLTextAreaElement).value).toBe('prod');
});

it('round-trips an environment value with newlines and an equals sign', async () => {
  const fetcher = accept();
  edit({ env: [{ name: 'PEM', value: 'x=y\nz' }] });
  fireEvent.click(screen.getByRole('button', { name: 'Reveal value of PEM' }));
  const value = screen.getByLabelText('Value of PEM') as HTMLTextAreaElement;
  expect(value.value).toBe('x=y\nz');
  fireEvent.change(value, { target: { value: `${value.value}!` } });
  const spec = await postedSpec(fetcher);
  expect(spec.env).toEqual([{ name: 'PEM', value: 'x=y\nz!' }]);
});

it('opens a new variable row for typing', () => {
  edit();
  fireEvent.click(screen.getByRole('button', { name: 'Add variable' }));
  expect(screen.getByLabelText('Value of variable 3').tagName).toBe('TEXTAREA');
  expect(screen.queryByLabelText('Value of SECRET')).toBeNull();
});

it('requires the typed container name before saving a change', () => {
  edit();
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  expect(save().disabled).toBe(true);
  confirmName('we');
  expect(save().disabled).toBe(true);
  confirmName('web');
  expect(save().disabled).toBe(false);
});

it('requires acknowledging a new bind mount', () => {
  edit();
  fireEvent.click(screen.getByRole('button', { name: 'Add mount' }));
  fireEvent.change(screen.getByLabelText('Mount 2 kind'), { target: { value: 'bind' } });
  fireEvent.change(screen.getByLabelText('Mount 2 source'), { target: { value: '/etc' } });
  fireEvent.change(screen.getByLabelText('Mount 2 target'), { target: { value: '/host-etc' } });
  confirmName('web');
  expect(save().disabled).toBe(true);
  expect(screen.queryByLabelText('This container will see host path /srv/data')).toBeNull();
  fireEvent.click(screen.getByLabelText('This container will see host path /etc'));
  expect(save().disabled).toBe(false);
});

it('asks to acknowledge a read-only bind made writable and sends its path', async () => {
  const fetcher = accept();
  edit({ mounts: [{ kind: 'bind', source: '/srv/data', target: '/data', read_only: true }, { kind: 'bind', source: '/srv/logs', target: '/logs', read_only: false }] });
  expect(screen.queryByLabelText(/This container will see host path/)).toBeNull();
  fireEvent.click(screen.getByLabelText('Mount 2 read-only'));
  expect(screen.queryByLabelText(/This container will see host path/)).toBeNull();
  fireEvent.click(screen.getByLabelText('Mount 1 read-only'));
  confirmName('web');
  expect(save().disabled).toBe(true);
  fireEvent.click(screen.getByLabelText('This container will see host path /srv/data'));
  expect(save().disabled).toBe(false);
  confirmName('web');
  await act(async () => { fireEvent.click(save()); });
  expect(JSON.parse(String((fetcher.mock.calls[0] as [unknown, RequestInit])[1].body)).acknowledge_binds).toEqual(['/srv/data']);
});

it('disables Save and names the settings it cannot carry', () => {
  edit({ unsupported: ['volumes_from', 'env_truncated'] });
  expect(screen.getByText(/mounts volumes from another container/)).toBeTruthy();
  expect(screen.getByText(/has more environment than the read could carry/)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  confirmName('web');
  expect(save().disabled).toBe(true);
});

it('posts the recreate with CSRF and expects and hands the command to the page', async () => {
  document.cookie = 'ky_csrf=test-token';
  const sent = vi.fn();
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => json({ id: 'cmd1', action: 'container.recreate', outcome: '' }, 202));
  vi.stubGlobal('fetch', fetcher);
  const { rerender } = edit({}, { onSent: sent });
  fireEvent.click(screen.getByRole('button', { name: 'Reveal value of SECRET' }));
  fireEvent.change(screen.getByLabelText('Value of SECRET'), { target: { value: 'next' } });
  confirmName('web');
  await act(async () => { fireEvent.click(save()); });
  expect(fetcher).toHaveBeenCalledTimes(1);
  const [url, init] = fetcher.mock.calls[0]!;
  expect(url).toBe(`${base}/containers/${id}/recreate`);
  expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('test-token');
  const body = JSON.parse(String(init?.body));
  expect(body.expects).toEqual({ image_id: imageID, created_unix: 7, state: 'running' });
  expect(body.confirm).toBe('web');
  expect(body.acknowledge_binds).toEqual([]);
  expect(body.spec).not.toHaveProperty('target');
  expect(body.spec.env).toEqual([{ name: 'SECRET', value: 'next' }, { name: 'MODE', value: 'prod' }]);
  expect(Object.keys(body.spec.labels)).toEqual(['a', 'b']);
  expect(body.spec.unsupported).toEqual([]);
  expect(sent).toHaveBeenCalledWith(expect.objectContaining({ id: 'cmd1' }));
  rerender(<ContainerConfigurationForm base={base} mode="edit" initial={config()} container={container} onSent={sent} pending />);
  expect(save().disabled).toBe(true);
  expect(screen.getByLabelText('Hostname').matches(':disabled')).toBe(true);
});

it('renders a settled command with its steps', () => {
  render(<CommandResult command={{ id: 'cmd1', action: 'container.recreate', outcome: 'failed', result: { code: 'step_failed', steps: [{ service: 'direct', step: 'start', outcome: 'failed', code: 'runtime_error', detail: '' }, { service: 'direct', step: 'rollback', outcome: 'succeeded', code: 'start_failed_rolled_back', detail: '' }], services: [] } }} org="a" endpoint="ep_1" current={id} />);
  const table = screen.getByRole('table');
  expect(within(table).getByText('rollback')).toBeTruthy();
  expect(within(table).getByText('The new container did not start; the previous one was restored.')).toBeTruthy();
});

it('maps refusals to fixed texts', async () => {
  const replies = [json({ code: 'invalid_spec', blockers: ['name_taken', 'image_unresolved', 'spec_invalid:ports', 'spec_invalid:nonsense'] }, 422), json({ error: 'raw server text' }, 501), json({ code: 'command_in_progress', error: 'raw server text' }, 409)];
  vi.stubGlobal('fetch', vi.fn(async () => replies.shift()!));
  edit();
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  confirmName('web');
  await act(async () => { fireEvent.click(save()); });
  const alert = screen.getByRole('alert');
  expect(alert.textContent).toContain('Another container already uses that name.');
  expect(alert.textContent).toContain("The image could not be resolved. Check the reference and the organization's registries.");
  expect(alert.textContent).toContain('The server refused the ports setting.');
  expect(alert.textContent).toContain('The server refused part of this configuration.');
  await act(async () => { fireEvent.click(save()); });
  expect(screen.getByRole('alert').textContent).toBe('Upgrade the host agent to enable editing.');
  await act(async () => { fireEvent.click(save()); });
  expect(screen.getByRole('alert').textContent).toBe('A recreate or run is already in progress on this host.');
  expect(document.body.textContent).not.toContain('raw server text');
});

it('runs a new container from an empty form once image and name are set', async () => {
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => json({ id: 'cmd2', action: 'container.run', outcome: '' }, 202));
  vi.stubGlobal('fetch', fetcher);
  render(<ContainerConfigurationForm base={base} mode="run" onSent={() => {}} />);
  const run = screen.getByRole('button', { name: 'Run container' }) as HTMLButtonElement;
  expect((screen.getByLabelText('Image reference') as HTMLInputElement).value).toBe('');
  expect(run.disabled).toBe(true);
  fireEvent.change(screen.getByLabelText('Container name'), { target: { value: 'api' } });
  fireEvent.change(screen.getByLabelText(/^Type the new container name/), { target: { value: 'api' } });
  expect(run.disabled).toBe(true);
  fireEvent.change(screen.getByLabelText('Image reference'), { target: { value: 'nginx:1' } });
  expect(run.disabled).toBe(false);
  await act(async () => { fireEvent.click(run); });
  const [url, init] = fetcher.mock.calls[0]!;
  expect(url).toBe(`${base}/containers`);
  const body = JSON.parse(String(init?.body));
  expect(body).not.toHaveProperty('expects');
  expect(body.confirm).toBe('api');
  expect(body.spec.name).toBe('api');
  expect(body.spec.image).toEqual({ reference: 'nginx:1', digest: '' });
});

const postedSpec = async (fetcher: ReturnType<typeof vi.fn>) => {
  confirmName('web');
  await act(async () => { fireEvent.click(save()); });
  return JSON.parse(String((fetcher.mock.calls[0] as [unknown, RequestInit])[1].body)).spec;
};
const accept = () => { const f = vi.fn(async (_i: RequestInfo | URL, _init?: RequestInit) => json({ id: 'c', action: 'container.recreate', outcome: '' }, 202)); vi.stubGlobal('fetch', f); return f; };

it('clears the image ID with the digest when pulling the current digest', async () => {
  const fetcher = accept();
  edit();
  fireEvent.click(screen.getByLabelText("Pull the reference's current digest"));
  const spec = await postedSpec(fetcher);
  expect(spec.image_id).toBe('');
  expect(spec.image.digest).toBe('');
});

it('says the host image is kept until pulling is ticked', () => {
  const kept = "The host's image is kept; tick to pull the reference's current digest instead.";
  edit({ image: { reference: 'nginx:1', digest: '' } });
  expect(screen.getByText(kept)).toBeTruthy();
  expect(screen.queryByText(/saving pulls/)).toBeNull();
  fireEvent.click(screen.getByLabelText("Pull the reference's current digest"));
  expect(screen.queryByText(kept)).toBeNull();
});

it('keeps Save disabled while a health check has no command', () => {
  edit();
  fireEvent.change(screen.getByRole('combobox', { name: 'Health check' }), { target: { value: 'command' } });
  confirmName('web');
  expect(save().disabled).toBe(true);
  fireEvent.change(screen.getByLabelText(/^Test, one argument per line/), { target: { value: 'CMD-SHELL\ncurl -f localhost' } });
  expect(save().disabled).toBe(false);
});

it('shows visible placeholders on row inputs', () => {
  edit({ env: [], ports: [], log: { driver: 'json-file', options: {} } });
  for (const noun of ['variable', 'port', 'device', 'option']) fireEvent.click(screen.getByRole('button', { name: `Add ${noun}` }));
  for (const text of ['NAME', 'value', 'container port', 'host port', 'host IP', 'host path', 'container path', 'permissions', 'option']) expect(screen.getAllByPlaceholderText(text).length).toBeGreaterThan(0);
  expect(screen.getByLabelText('Port 1 host IP').getAttribute('placeholder')).toBe('host IP');
});

it('keeps Save disabled while a row is incomplete', () => {
  edit();
  fireEvent.click(screen.getByRole('button', { name: 'Add variable' }));
  confirmName('web');
  expect(save().disabled).toBe(true);
  expect(screen.getByText('Complete every row: variable names, container ports, device paths and the health check command.')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Remove variable 3' }));
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  expect(save().disabled).toBe(false);
});

it('accepts a typed -1 and an empty draft in numeric fields', async () => {
  const fetcher = accept();
  edit({ resources: { nano_cpus: 0, memory_bytes: 268435456, memory_swap_bytes: 536870912, pids_limit: 0 } });
  const swap = screen.getByLabelText('Memory and swap (MiB)') as HTMLInputElement;
  expect(swap.type).toBe('text');
  fireEvent.change(swap, { target: { value: '' } });
  expect(swap.value).toBe('');
  fireEvent.change(swap, { target: { value: '-' } });
  expect(swap.value).toBe('-');
  expect(save().disabled).toBe(true);
  fireEvent.change(swap, { target: { value: '-1' } });
  const spec = await postedSpec(fetcher);
  expect(spec.resources.memory_swap_bytes).toBe(-1);
});

it('links to the new container only after a success that names another ID', () => {
  const next = 'e'.repeat(64);
  const cmd = (outcome: string, container_id: string): DirectCommand => ({ id: 'c', action: 'container.recreate', outcome, result: { steps: [], services: [{ service: 'direct', container_id, image_id: imageID, created_unix: 1 }] } });
  const link = () => screen.queryByRole('link', { name: 'Open the new container' });
  render(<CommandResult command={cmd('succeeded', next)} org="a" endpoint="ep_1" current={id} />);
  expect(link()?.getAttribute('href')).toBe(`/organizations/a/endpoints/ep_1/containers/${next}?tab=configuration`);
  cleanup();
  render(<CommandResult command={cmd('failed', next)} org="a" endpoint="ep_1" current={id} />);
  expect(link()).toBeNull();
  cleanup();
  render(<CommandResult command={cmd('succeeded', id)} org="a" endpoint="ep_1" current={id} />);
  expect(link()).toBeNull();
});

it('shows the privileged_disabled refusal text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ code: 'invalid_spec', blockers: ['privileged_disabled'] }, 422)));
  edit();
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  confirmName('web');
  await act(async () => { fireEvent.click(save()); });
  expect(screen.getByRole('alert').textContent).toBe('Host-level settings (privileged, devices, security options, extra capabilities, host networking or system paths) are disabled on this server. Set KY_CONTAINER_ALLOW_PRIVILEGED to allow them.');
});

it('round-trips a read-only tmpfs mount', async () => {
  const fetcher = accept();
  edit({ mounts: [{ kind: 'tmpfs', source: '', target: '/scratch', read_only: true }] });
  expect((screen.getByLabelText(/read-only/) as HTMLInputElement).checked).toBe(true);
  fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'box' } });
  const spec = await postedSpec(fetcher);
  expect(spec.mounts).toEqual([{ kind: 'tmpfs', source: '', target: '/scratch', read_only: true }]);
});
