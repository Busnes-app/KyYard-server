import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ACKNOWLEDGEMENTS, ApplicationMigration, ASSUMPTIONS, CHECKLIST_STEPS, MIGRATION_CODES, MIGRATION_ERRORS, findingText } from './ApplicationMigration';
import { messages } from './ApplicationPreflight';
import { STEP_CODES } from './ApplicationDeploymentPlan';
import { unsupportedNames } from './ApplicationInspection';
import migrationCodes from '../migration-codes.json';
import protocolCodes from '../protocol-codes.json';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const sorted = (xs: string[]) => [...xs].sort();
const docker = { id: 'i1', application_id: 'app', endpoint_id: 'ep_d', endpoint_name: 'docker-1', project: 'shop', revision: 1, current_revision: 0, previous_revision: 0, mapping_version: 2, container_count: 2, containers: [] };
const cluster = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['shop'] };
const inventory = { snapshot: { kubernetes: { storage_classes: [{ name: 'standard', default: true }, { name: 'fast', default: false }] } } };
const findings = (volume: string) => [
  { axis: 'storage', class: volume, code: 'volume_named', detail: 'data' },
  { axis: 'networking', class: 'supported', code: 'networking_supported' },
  { axis: 'ports', class: 'supported', code: 'port_unpublished' },
  { axis: 'flags', class: 'blocked', code: 'flag_blocked', detail: 'privileged' },
];
const migration = (over: Record<string, unknown> = {}) => ({
  id: 'm1', application_id: 'app', application_name: 'shop', destination_endpoint_id: 'ep_k', namespace: 'shop', status: 'analyzed', ready: false, role: 'source', choices: { volumes: {} },
  report: { version: 1, ready: false, services: [{ name: 'db', class: 'blocked', findings: findings('operator_choice_required') }], checklist: [{ code: 'grant_namespace', commands: ['kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline'] }, { code: 'create_destination' }], assumptions: ['volume_size_unknown'] },
  ...over,
});

// Every code the Go vocabularies define has exactly one sentence here: the fixtures are
// generated from internal/migration and internal/agent/protocol.
it('has a sentence for every generated code, and no other', () => {
  expect(sorted(Object.keys(MIGRATION_CODES))).toEqual(sorted(migrationCodes.codes));
  expect(sorted(Object.keys(CHECKLIST_STEPS))).toEqual(sorted(migrationCodes.checklist));
  expect(sorted(Object.keys(ASSUMPTIONS))).toEqual(sorted(migrationCodes.assumptions));
  expect(sorted(Object.keys(ACKNOWLEDGEMENTS))).toEqual(sorted(migrationCodes.acknowledgeable));
  expect(messages.agent_claims_unsupported).toBe("The cluster agent must be upgraded before claims can be applied: set the agent Deployment's image to the current pinned digest, then plan again.");
  expect(messages.storage_class_unknown).toBeTruthy();
  expect(sorted(Object.keys(STEP_CODES))).toEqual(sorted(protocolCodes.step_codes));
  expect(sorted(Object.keys(unsupportedNames))).toEqual(sorted(protocolCodes.unsupported_codes));
  for (const code of ['namespace_unknown', 'runtime_unsupported', 'migration_open', 'storage_class_unknown', 'size_invalid', 'volume_unknown', 'application_name_taken', 'migration_not_ready', 'migration_state', 'migration_stale']) {
    expect(MIGRATION_ERRORS[code]).toBeTruthy();
  }
});

it('renders a finding with its parameter and nothing for an unknown code', () => {
  expect(findingText({ axis: 'flags', class: 'blocked', code: 'flag_blocked', detail: 'privileged' })).toBe(`${MIGRATION_CODES.flag_blocked} (runs privileged)`);
  expect(findingText({ axis: 'ports', class: 'supported', code: 'port_published', detail: '8080/tcp' })).toBe(`${MIGRATION_CODES.port_published} (8080/tcp)`);
  expect(findingText({ axis: 'x', class: 'blocked', code: '<b>secret-canary</b>' })).toBe('');
});

it('offers an administrator the analysis of a Docker source and posts the cluster and namespace', async () => {
  document.cookie = 'ky_csrf=csrf';
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') return json(migration(), 201);
    return String(url).endsWith('/migration') ? json({ error: 'not found' }, 404) : json([cluster]);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  await screen.findByRole('option', { name: 'prod' });
  fireEvent.change(screen.getByLabelText('Destination cluster'), { target: { value: 'ep_k' } });
  const analyze = screen.getByRole('button', { name: 'Analyze' });
  expect(analyze.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Destination namespace'), { target: { value: 'shop' } });
  fireEvent.click(analyze);
  await vi.waitFor(() => expect(fetcher.mock.calls.some((c) => c[1]?.method === 'POST')).toBe(true));
  const post = fetcher.mock.calls.find((c) => c[1]?.method === 'POST');
  expect(String(post?.[0])).toBe('/app/migration');
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ destination_endpoint_id: 'ep_k', namespace: 'shop' });
});

it('explains a cluster whose manifest grants no namespace instead of an empty select', async () => {
  const fetcher = vi.fn(async (url: RequestInfo | URL) => String(url).endsWith('/migration') ? json({ error: 'not found' }, 404) : json([{ ...cluster, deploy_namespaces: [] }]));
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  await screen.findByRole('option', { name: 'prod' });
  fireEvent.change(screen.getByLabelText('Destination cluster'), { target: { value: 'ep_k' } });
  expect(await screen.findByText("This cluster's manifest grants no namespace yet. Regenerate it below and apply it.")).toBeTruthy();
  expect(screen.queryByLabelText('Destination namespace')).toBeNull();
  expect(screen.getByRole('button', { name: 'Analyze' }).hasAttribute('disabled')).toBe(true);
});

it('renders nothing for a response it cannot read', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ role: 'source', status: 'analyzed', report: '<b>secret-canary</b>' })));
  const { container } = render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  await vi.waitFor(() => expect(container.textContent).toBe(''));
});

it('renders nothing for a member who may not migrate, or for a cluster instance with no migration', async () => {
  for (const [instance, admin] of [[docker, false], [{ ...docker, namespace: 'shop' }, true]] as const) {
    const fetcher = vi.fn(async () => json({}, 404));
    vi.stubGlobal('fetch', fetcher);
    const { container } = render(<ApplicationMigration base="/app" org="a" env="e" instance={instance} admin={admin} onOpen={vi.fn()} />);
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalled());
    await vi.waitFor(() => expect(container.textContent).toBe(''));
    cleanup();
  }
});

it('shows the report, takes storage choices from the cluster, and holds the destination until ready', async () => {
  document.cookie = 'ky_csrf=csrf';
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PUT') return json({ code: 'storage_class_unknown', error: 'secret-canary' }, 400);
    return String(url).endsWith('/inventory') ? json(inventory) : json(migration());
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  const table = await screen.findByRole('table');
  expect(within(table).getByText(`${MIGRATION_CODES.volume_named} (data)`)).toBeTruthy();
  expect(within(table).getAllByText('Blocked').length).toBe(1);
  expect(within(table).getByText('Choice required')).toBeTruthy();
  expect(screen.getByText(ASSUMPTIONS.volume_size_unknown)).toBeTruthy();
  expect(screen.getByText('kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Create destination' }).hasAttribute('disabled')).toBe(true);
  const storage = await screen.findByLabelText('StorageClass for data');
  await vi.waitFor(() => expect([...storage.querySelectorAll('option')].map((o) => o.textContent)).toEqual(['Cluster default (standard)', 'standard', 'fast']));
  fireEvent.change(storage, { target: { value: 'fast' } });
  fireEvent.change(screen.getByLabelText('Size for data'), { target: { value: '10Gi' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save choices' }));
  expect((await screen.findByRole('alert')).textContent).toBe(MIGRATION_ERRORS.storage_class_unknown);
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(String(put?.[0])).toBe('/app/migration/choices');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ volumes: { data: { storage_class: 'fast', size: '10Gi', access_mode: 'ReadWriteOnce' } }, acknowledged: [] });
  expect(document.body.textContent).not.toContain('secret-canary');
});

it('names each destination and takes the acknowledgements with the storage choices', async () => {
  document.cookie = 'ky_csrf=csrf';
  const report = { version: 1, ready: false, services: [{ name: 'db', class: 'operator_choice_required', findings: [
    { axis: 'networking', class: 'operator_choice_required', code: 'network_references', detail: 'shop-on-prod-db' },
    { axis: 'ports', class: 'operator_choice_required', code: 'port_unpublished' },
  ] }], checklist: [{ code: 'update_references', commands: ['db → shop-on-prod-db'] }], assumptions: [] };
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PUT') return json(migration({ report }));
    return String(url).endsWith('/inventory') ? json(inventory) : json(migration({ report }));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  expect(await screen.findByText(`${MIGRATION_CODES.network_references} (shop-on-prod-db)`)).toBeTruthy();
  expect(screen.getByText('db → shop-on-prod-db')).toBeTruthy();
  expect(screen.queryByLabelText('StorageClass for data')).toBeNull();
  fireEvent.click(screen.getByLabelText(ACKNOWLEDGEMENTS.network_references));
  fireEvent.click(screen.getByRole('button', { name: 'Save choices' }));
  await vi.waitFor(() => expect(fetcher.mock.calls.some((c) => c[1]?.method === 'PUT')).toBe(true));
  expect(JSON.parse(String(fetcher.mock.calls.find((c) => c[1]?.method === 'PUT')?.[1]?.body))).toEqual({ volumes: {}, acknowledged: ['network_references'] });
});

it('links the created destination, takes the confirmations with a note, and says an abandoned destination stays', async () => {
  document.cookie = 'ky_csrf=csrf';
  const open = vi.fn();
  const created = migration({ status: 'destination_created', ready: true, destination_application_id: 'dest', destination_application_name: 'shop on prod' });
  let abandoned = false;
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'DELETE') { abandoned = true; return json({ ...created, status: 'abandoned', destination_kept: true }); }
    return json(abandoned ? { ...created, status: 'abandoned' } : created);
  });
  vi.stubGlobal('fetch', fetcher);
  vi.stubGlobal('confirm', () => true);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={open} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Open shop on prod' }));
  expect(open).toHaveBeenCalledWith('dest');
  expect(screen.queryByRole('button', { name: 'Create destination' })).toBeNull();
  const confirm = screen.getByRole('button', { name: 'Confirm validation' });
  expect(confirm.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Note for confirm validation'), { target: { value: 'orders page answers' } });
  fireEvent.click(confirm);
  await vi.waitFor(() => expect(fetcher.mock.calls.some((c) => String(c[0]) === '/app/migration/validated')).toBe(true));
  expect(JSON.parse(String(fetcher.mock.calls.find((c) => String(c[0]) === '/app/migration/validated')?.[1]?.body))).toEqual({ note: 'orders page answers' });
  fireEvent.click(await screen.findByRole('button', { name: 'Abandon migration' }));
  expect((await screen.findByRole('alert')).textContent).toContain('The destination application stays');
  await vi.waitFor(() => expect(screen.queryByRole('button', { name: 'Abandon migration' })).toBeNull());
});

it('shows a destination where it came from', async () => {
  const open = vi.fn();
  vi.stubGlobal('fetch', vi.fn(async () => json(migration({ role: 'destination', status: 'validated' }))));
  render(<ApplicationMigration base="/dest" org="a" env="e" instance={{ ...docker, namespace: 'shop' }} admin={false} onOpen={open} />);
  expect(await screen.findByText('Migration destination of shop · Validated')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Open shop' }));
  expect(open).toHaveBeenCalledWith('app');
});
