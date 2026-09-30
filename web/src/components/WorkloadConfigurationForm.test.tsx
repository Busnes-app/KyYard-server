import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { WorkloadConfigurationForm, WorkloadResult } from './WorkloadConfigurationForm';
import type { WorkloadConfiguration } from '../tenant';
import { commandLine } from './workloadTexts';

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
  await waitFor(() => expect(onSent).toHaveBeenCalledWith({ id: 'k7', action: 'workload.apply', outcome: '' }, { namespace: 'shop', kind: 'deployment', name: 'web' }));
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
  [json({ code: 'command_in_progress' }, 409), 'A workload apply or run, or a container change, on this cluster is waiting for its result.'],
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

it('words a Pod Security precondition without the raw detail', () => {
  render(<WorkloadResult command={{ id: 'k8', action: 'workload.apply', outcome: 'denied', result: { code: 'step_failed', steps: [
    { service: 'workload', step: 'precondition', outcome: 'denied', code: 'pod_security', detail: 'privileged' },
  ], services: [] } }} />);
  expect(screen.getByText('This namespace does not enforce Pod Security baseline or restricted; KyYard refuses to change workloads there.')).toBeTruthy();
  expect(document.body.textContent).not.toContain('privileged');
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

const runForm = (namespaces = ['shop', 'billing'], onSent = vi.fn()) => { render(<WorkloadConfigurationForm base={base} mode="run" namespaces={namespaces} onSent={onSent} />); return onSent; };
const runButton = () => screen.getByRole('button', { name: 'Run workload' });

it('runs a new Deployment only with namespace, name, image and the typed name, posting the create body', async () => {
  const bodies: unknown[] = [];
  const urls: string[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    urls.push(String(input));
    expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('csrf-r');
    bodies.push(JSON.parse(String(init?.body)));
    return json({ id: 'k8', action: 'workload.run', reference: 'shop/deployment/fresh', outcome: '' }, 202);
  }));
  document.cookie = 'ky_csrf=csrf-r';
  const onSent = runForm();
  expect(screen.getByRole('region', { name: 'Run a workload' })).toBeTruthy();
  expect(screen.getByLabelText('Replicas')).toHaveProperty('value', '1');
  expect(screen.queryByText(/Changes:|No changes\./)).toBeNull();
  const steps: [string, string][] = [['Workload name', 'fresh'], ['Name of container 1', 'web'], ['Image of container 1', 'nginx:1.30'], ['Type the new workload name to confirm', 'fresh']];
  for (const [label, value] of steps) {
    expect(runButton().hasAttribute('disabled')).toBe(true);
    fireEvent.change(screen.getByLabelText(label), { target: { value } });
  }
  expect(runButton().hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByRole('combobox', { name: 'Namespace' }), { target: { value: 'shop' } });
  fireEvent.change(screen.getByLabelText('Replicas'), { target: { value: '2' } });
  fireEvent.click(screen.getByRole('button', { name: 'Add container 1 variable' }));
  fireEvent.change(screen.getByLabelText('Container 1 variable 1 name'), { target: { value: 'K' } });
  fireEvent.change(screen.getByLabelText('Value of K'), { target: { value: 'v' } });
  expect(runButton().hasAttribute('disabled')).toBe(false);
  fireEvent.click(runButton());
  await waitFor(() => expect(onSent).toHaveBeenCalledWith({ id: 'k8', action: 'workload.run', reference: 'shop/deployment/fresh', outcome: '' }, { namespace: 'shop', kind: 'deployment', name: 'fresh' }));
  expect(urls).toEqual([`${base}/workloads`]);
  expect(bodies[0]).toEqual({
    confirm: 'fresh',
    spec: {
      target: { namespace: 'shop', kind: 'deployment', name: 'fresh' }, resource_version: '', replicas: 2, strategy: 'RollingUpdate', paused: false,
      containers: [{ name: 'web', image: 'nginx:1.30', image_id: '', command: [], args: [], env: [{ name: 'K', value: 'v' }], resources: { cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' } }],
      init_containers: [], env_from: [], managed: false, unsupported: [],
    },
  });
  expect(bodies[0]).not.toHaveProperty('spec.target.uid');
});

it('holds Run for a name that is not a DNS label or a confirm that differs', () => {
  runForm(['shop']);
  expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveProperty('value', 'shop');
  fireEvent.change(screen.getByLabelText('Name of container 1'), { target: { value: 'web' } });
  fireEvent.change(screen.getByLabelText('Image of container 1'), { target: { value: 'nginx:1.30' } });
  for (const bad of ['Fresh', 'fresh_1', '-fresh', 'a'.repeat(64)]) {
    fireEvent.change(screen.getByLabelText('Workload name'), { target: { value: bad } });
    fireEvent.change(screen.getByLabelText('Type the new workload name to confirm'), { target: { value: bad } });
    expect(runButton().hasAttribute('disabled')).toBe(true);
  }
  expect(screen.getByText(/lower-case DNS label/)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Workload name'), { target: { value: 'fresh' } });
  fireEvent.change(screen.getByLabelText('Type the new workload name to confirm'), { target: { value: 'fres' } });
  expect(runButton().hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Type the new workload name to confirm'), { target: { value: 'fresh' } });
  expect(runButton().hasAttribute('disabled')).toBe(false);
});

it('adds and removes containers in run mode and requires each a name and an image', () => {
  runForm(['shop']);
  fireEvent.change(screen.getByLabelText('Workload name'), { target: { value: 'fresh' } });
  fireEvent.change(screen.getByLabelText('Type the new workload name to confirm'), { target: { value: 'fresh' } });
  fireEvent.change(screen.getByLabelText('Name of container 1'), { target: { value: 'web' } });
  fireEvent.change(screen.getByLabelText('Image of container 1'), { target: { value: 'nginx:1.30' } });
  expect(screen.queryByRole('button', { name: 'Remove container 1' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Add a container' }));
  expect(runButton().hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Name of container 2'), { target: { value: 'web' } });
  fireEvent.change(screen.getByLabelText('Image of container 2'), { target: { value: 'busybox:1' } });
  expect(runButton().hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Name of container 2'), { target: { value: 'sidecar' } });
  expect(runButton().hasAttribute('disabled')).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: 'Remove container 1' }));
  expect(screen.getByLabelText('Name of container 1')).toHaveProperty('value', 'sidecar');
  expect(screen.queryByLabelText('Name of container 2')).toBeNull();
});

it('does not offer adding or removing containers when editing', () => {
  form();
  expect(screen.queryByRole('button', { name: 'Add a container' })).toBeNull();
  expect(screen.queryByRole('button', { name: /^Remove container/ })).toBeNull();
  expect(screen.queryByLabelText('Workload name')).toBeNull();
});

it.each([
  [json({ code: 'invalid_spec', blockers: ['name_taken'] }, 422), 'A workload with this name already exists in the namespace. Choose another name.'],
  [json({ code: 'namespace_not_granted' }, 422), 'This namespace is not granted to KyYard on this cluster.'],
  [json({ code: 'command_in_progress' }, 409), 'A workload apply or run, or a container change, on this cluster is waiting for its result.'],
  [json({ error: 'no inventory' }, 404), 'The cluster has not reported its inventory yet. Try again shortly.'],
])('renders a refused run as fixed text', async (response, text) => {
  vi.stubGlobal('fetch', vi.fn(async () => response));
  runForm(['shop']);
  fireEvent.change(screen.getByLabelText('Workload name'), { target: { value: 'fresh' } });
  fireEvent.change(screen.getByLabelText('Name of container 1'), { target: { value: 'web' } });
  fireEvent.change(screen.getByLabelText('Image of container 1'), { target: { value: 'nginx:1.30' } });
  fireEvent.change(screen.getByLabelText('Type the new workload name to confirm'), { target: { value: 'fresh' } });
  fireEvent.click(runButton());
  expect((await screen.findByRole('alert')).textContent).toBe(text);
});

it('words a run command and a name_taken step', () => {
  expect(commandLine({ action: 'workload.run', outcome: 'denied', detail: 'name_taken' })).toBe('Run refused. A workload with this name already exists in the namespace. Choose another name.');
  render(<WorkloadResult command={{ id: 'k8', action: 'workload.run', outcome: 'denied', result: { code: 'step_failed', steps: [
    { service: 'workload', step: 'precondition', outcome: 'denied', code: 'name_taken', detail: '' },
  ], services: [] } }} />);
  expect(screen.getByText('A workload with this name already exists in the namespace. Choose another name.')).toBeTruthy();
});
