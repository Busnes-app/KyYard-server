import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { WorkloadConfigurationForm, WorkloadResult } from './WorkloadConfigurationForm';
import type { WorkloadConfiguration } from '../tenant';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const base = '/api/organizations/a/endpoints/ep_k';
const SECRET = 's3cr3t-literal';
const configuration = (over: Partial<WorkloadConfiguration> = {}): WorkloadConfiguration => ({
  target: { namespace: 'shop', kind: 'deployment', name: 'web' }, observed_at: '2026-09-29T10:00:00Z', resource_version: '41', replicas: 3, paused: false, strategy: 'RollingUpdate',
  containers: [{ name: 'web', image: 'nginx:1.29', image_id: '', command: [], args: ['--port', '80'], env: [{ name: 'MODE', value: SECRET }, { name: 'DB_PASSWORD', secret_ref: 'db/password' }, { name: 'LEVEL', config_map_ref: 'app/level' }], resources: { cpu_request: '100m', cpu_limit: '', memory_request: '128Mi', memory_limit: '256Mi' } }],
  init_containers: [], env_from: [], managed: false, unsupported: [], ...over,
});
const form = (initial = configuration(), onSent = vi.fn()) => { render(<WorkloadConfigurationForm base={base} initial={initial} onSent={onSent} />); return onSent; };
const save = () => screen.getByRole('button', { name: 'Save and apply' });

it('round-trips the read with no changes and keeps Save disabled', () => {
  form();
  expect(screen.getByText('No changes.')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  expect(save().hasAttribute('disabled')).toBe(true);
  expect(screen.getByLabelText('Replicas')).toHaveProperty('value', '3');
  expect(screen.getByLabelText('Arguments of web')).toHaveProperty('value', '--port\n80');
});

it('keeps a literal value out of the DOM until revealed and shows references read-only', () => {
  const { container } = render(<WorkloadConfigurationForm base={base} initial={configuration()} onSent={vi.fn()} />);
  expect(container.innerHTML).not.toContain(SECRET);
  fireEvent.click(screen.getByRole('button', { name: 'Reveal value of MODE' }));
  const value = screen.getByLabelText('Value of MODE');
  expect(value).toHaveProperty('value', SECRET);
  expect(value.getAttribute('autocomplete')).toBe('off');
  expect(value.hasAttribute('data-1p-ignore')).toBe(true);
  expect(value.getAttribute('data-lpignore')).toBe('true');
  expect(value.hasAttribute('data-bwignore')).toBe(true);
  fireEvent.click(screen.getByRole('button', { name: 'Hide value of MODE' }));
  expect(container.innerHTML).not.toContain(SECRET);
  expect(screen.getByText('Secret db/password')).toBeTruthy();
  expect(screen.getByText('ConfigMap app/level')).toBeTruthy();
  expect(screen.queryByLabelText('Value of DB_PASSWORD')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Remove web variable 2' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Remove web variable 1' })).toBeTruthy();
});

it('posts apply with the read resource_version, the whole spec and the typed name', async () => {
  const bodies: unknown[] = [];
  const urls: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    urls.push(String(input));
    expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('csrf-f');
    bodies.push(JSON.parse(String(init?.body)));
    return json({ id: 'k7', action: 'workload.apply', outcome: '' }, 202);
  }));
  document.cookie = 'ky_csrf=csrf-f';
  const onSent = form();
  fireEvent.change(screen.getByLabelText('Image of web'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Replicas'), { target: { value: '4' } });
  expect(screen.getByText('Changes: replicas, containers.web.image')).toBeTruthy();
  expect(save().hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  fireEvent.click(save());
  await waitFor(() => expect(onSent).toHaveBeenCalledWith({ id: 'k7', action: 'workload.apply', outcome: '' }));
  expect(urls).toEqual([`${base}/workloads/shop/deployment/web/apply`]);
  const c = configuration();
  expect(bodies[0]).toEqual({
    resource_version: '41', confirm: 'web',
    spec: { resource_version: '41', replicas: 4, paused: false, strategy: 'RollingUpdate', init_containers: [], env_from: [], unsupported: [], containers: [{ ...c.containers[0], image: 'nginx:1.30' }] },
  });
});

it('disables Save for unsupported settings and has no replicas on a DaemonSet', () => {
  form(configuration({ target: { namespace: 'shop', kind: 'daemonset', name: 'web' }, replicas: undefined, unsupported: ['env_field_ref'] }));
  expect(screen.queryByLabelText('Replicas')).toBeNull();
  expect(screen.getByText('has environment taken from pod fields')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Image of web'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  expect(save().hasAttribute('disabled')).toBe(true);
});

it('holds Save while replicas are malformed or out of range', () => {
  form();
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  for (const bad of ['1001', '-1', '2.5', 'x']) {
    fireEvent.change(screen.getByLabelText('Replicas'), { target: { value: bad } });
    expect(save().hasAttribute('disabled')).toBe(true);
  }
  fireEvent.change(screen.getByLabelText('Replicas'), { target: { value: '0' } });
  expect(save().hasAttribute('disabled')).toBe(false);
});

it.each([
  [json({ code: 'command_in_progress' }, 409), 'A workload apply or container change on this cluster is waiting for its result.'],
  [json({ code: 'application_managed' }, 409), 'Managed by a KyYard application. Edit it there.'],
  [json({ code: 'namespace_not_granted' }, 422), 'This namespace is not granted to KyYard on this cluster.'],
  [json({ code: 'invalid_spec', blockers: ['configuration_incomplete', 'spec_invalid:replicas', 'spec_invalid:nonsense'] }, 422), 'This workload has settings the form cannot carry; nothing was changed. The server refused the replicas setting. The server refused part of this configuration.'],
  [json({ error: 'Upgrade the cluster agent to configure workloads' }, 501), 'Upgrade the cluster agent and re-apply the manifest to enable this.'],
])('renders a refused apply as fixed text', async (response, text) => {
  vi.stubGlobal('fetch', vi.fn(async () => response));
  form();
  fireEvent.change(screen.getByLabelText('Image of web'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  fireEvent.click(save());
  expect((await screen.findByRole('alert')).textContent).toBe(text);
});

it('shows a settled apply with the workload step texts', () => {
  render(<WorkloadResult command={{ id: 'k7', action: 'workload.apply', outcome: 'denied', result: { code: 'step_failed', steps: [
    { service: 'workload', step: 'precondition', outcome: 'denied', code: 'conflict', detail: '' },
    { service: 'workload', step: 'apply', outcome: 'skipped', detail: '' },
  ], services: [] } }} />);
  expect(screen.getByText('The cluster refused the change; nothing was applied.')).toBeTruthy();
  expect(screen.getByText('The workload changed since you read it. Read again.')).toBeTruthy();
  expect(screen.getAllByRole('row')).toHaveLength(3);
});

it('offers Read again after a lost connection and never resubmits', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
  const onReread = vi.fn();
  render(<WorkloadConfigurationForm base={base} initial={configuration()} onSent={vi.fn()} onReread={onReread} />);
  expect(screen.queryByRole('button', { name: 'Read again' })).toBeNull();
  fireEvent.change(screen.getByLabelText('Image of web'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Type the workload name web to confirm'), { target: { value: 'web' } });
  fireEvent.click(save());
  expect((await screen.findByRole('alert')).textContent).toContain("the workload's Activity tab");
  expect(save().hasAttribute('disabled')).toBe(true);
  fireEvent.click(screen.getByRole('button', { name: 'Read again' }));
  expect(onReread).toHaveBeenCalledOnce();
});

it('offers Read again after a conflict result', () => {
  render(<WorkloadConfigurationForm base={base} initial={configuration()} onSent={vi.fn()} onReread={vi.fn()} conflict />);
  expect(screen.getByRole('button', { name: 'Read again' })).toBeTruthy();
});

it('words a timed_out apply', () => {
  render(<WorkloadResult command={{ id: 'k9', action: 'workload.apply', outcome: 'timed_out' }} />);
  expect(screen.getByText("The cluster agent did not answer in time; check the cluster's Activity tab before trying again.")).toBeTruthy();
});

it('shows imported Secret names read-only', () => {
  form(configuration({ env_from: ['db-credentials'] }));
  expect(screen.getByText('Imported from: db-credentials')).toBeTruthy();
  expect(screen.queryByDisplayValue('db-credentials')).toBeNull();
});
