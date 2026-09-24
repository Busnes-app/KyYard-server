import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationUpdates } from './ApplicationUpdates';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const local = `sha256:${'1234567890ab'}${'c'.repeat(52)}`;
const remote = `sha256:${'fedcba987654'}${'d'.repeat(52)}`;
const row = (over: Record<string, string>) => ({ service: 'web', reference: 'nginx:1', local_digest: local, remote_digest: remote, verdict: 'current', detail: '', checked_at: '2026-09-23T12:00:00Z', ...over });
const body = (services: unknown[]) => ({ instance_id: 'i', mapping_version: 2, services });
const props = { base: '/app', instanceID: 'i', mappingVersion: 2, project: 'shop', latestRevision: 4, onPlanned: () => {} };
const openPanel = () => fireEvent.click(screen.getByRole('button', { name: 'Updates' }));
function stub(get: unknown, post?: Response) {
  const fetcher = vi.fn(async (_url: string, init?: RequestInit) => init?.method === 'POST' ? post ?? new Response(JSON.stringify(get)) : new Response(JSON.stringify(get)));
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

it('renders nothing without a mapping', () => {
  const fetcher = stub(body([]));
  const { container } = render(<ApplicationUpdates {...props} mappingVersion={0} />);
  expect(container.textContent).toBe('');
  expect(fetcher).not.toHaveBeenCalled();
});
it('shows the empty state and a check button', async () => {
  stub(body([]));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('No update check yet.');
  expect(screen.getByRole('button', { name: 'Check for updates' })).toBeTruthy();
});
it('labels each verdict with fixed text and short digests', async () => {
  stub(body([
    row({ service: 'a', verdict: 'current' }),
    row({ service: 'b', verdict: 'update_available' }),
    row({ service: 'c', verdict: 'pinned', remote_digest: '' }),
    row({ service: 'd', verdict: 'unknown_local', local_digest: '' }),
  ]));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Up to date');
  expect(screen.getByText('Update available')).toBeTruthy();
  expect(screen.getByText('Pinned')).toBeTruthy();
  expect(screen.getByText('Unknown on host')).toBeTruthy();
  expect(screen.getAllByText('1234567890ab').length).toBeGreaterThan(0);
  expect(screen.getAllByText('fedcba987654').length).toBeGreaterThan(0);
  expect(document.body.textContent).not.toContain('c'.repeat(52));
});
it('maps every registry error detail to fixed text and never renders the raw detail', async () => {
  const details: Record<string, string> = {
    not_configured: 'No registry entry for this host and anonymous pulls are off.',
    unauthorized: 'The registry refused the credentials.',
    not_found: 'The image was not found in the registry.',
    rate_limited: 'The registry rate limit was reached; try later.',
    private_destination: 'The registry is on a private address this organization may not reach.',
    unavailable: 'The registry could not be reached.',
  };
  stub(body([...Object.keys(details).map((d, n) => row({ service: `s${n}`, verdict: 'registry_error', detail: d, remote_digest: '' })), row({ service: 'z', verdict: 'registry_error', detail: 'secret-canary', remote_digest: '' })]));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText(details.unauthorized);
  for (const text of Object.values(details)) expect(screen.getByText(text)).toBeTruthy();
  expect(screen.getAllByText('Registry error').length).toBe(7);
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('rate_limited');
});
it('posts the check with the CSRF header and renders the new rows', async () => {
  document.cookie = 'ky_csrf=csrf';
  let checked = false;
  const fetcher = vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'POST') { checked = true; return new Response(JSON.stringify(body([row({ verdict: 'update_available' })]))); }
    return new Response(JSON.stringify(body(checked ? [row({ verdict: 'update_available' })] : [])));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('No update check yet.');
  fireEvent.click(screen.getByRole('button', { name: 'Check for updates' }));
  await screen.findByText('Update available');
  const post = fetcher.mock.calls.find((c) => c[1]?.method === 'POST');
  expect(post?.[0]).toBe('/app/updates/check');
  expect(new Headers(post?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
});
it.each([
  [403, 'tenant_access_denied', 'Only administrators and developers can check for updates.'],
  [409, 'check_in_progress', 'A check is already running.'],
  [409, 'mapping_required', 'Map the services first.'],
  [409, 'adoption_changed', 'Adoption changed. Refresh applications before checking.'],
])('maps %i %s to fixed text', async (status, code, text) => {
  stub(body([]), new Response(JSON.stringify({ error: 'secret-canary', code }), { status }));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('No update check yet.');
  fireEvent.click(screen.getByRole('button', { name: 'Check for updates' }));
  await screen.findByText(text);
  expect(document.body.textContent).not.toContain('secret-canary');
});
it('hides rows cached for another instance', async () => {
  stub({ instance_id: 'other', mapping_version: 2, services: [row({})] });
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('No update check yet.');
  expect(screen.queryByText('Up to date')).toBeNull();
});
it('requests nothing until opened', async () => {
  const fetcher = stub(body([]));
  render(<ApplicationUpdates {...props} />);
  expect(fetcher).not.toHaveBeenCalled();
  expect(screen.queryByRole('button', { name: 'Check for updates' })).toBeNull();
  openPanel();
  await screen.findByText('No update check yet.');
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0][0]).toBe('/app/updates');
  fireEvent.click(screen.getByRole('button', { name: 'Close updates' }));
  expect(screen.queryByText('No update check yet.')).toBeNull();
});
it('keeps the previous rows while the post-check read is in flight', async () => {
  let release: (r: Response) => void = () => {};
  let gets = 0;
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'POST') return new Response(JSON.stringify(body([row({ verdict: 'update_available' })])));
    if (++gets === 1) return new Response(JSON.stringify(body([row({})])));
    return new Promise<Response>((resolve) => { release = resolve; });
  }));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Up to date');
  fireEvent.click(screen.getByRole('button', { name: 'Check for updates' }));
  await vi.waitFor(() => expect(gets).toBe(2));
  expect(screen.getByText('Up to date')).toBeTruthy();
  expect(screen.queryByText('Loading…')).toBeNull();
  release(new Response(JSON.stringify(body([row({ verdict: 'update_available' })]))));
  await screen.findByText('Update available');
});

const planUpdate = () => {
  fireEvent.change(screen.getByLabelText('Confirm update project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan update' }));
};
it('offers no plan update without an available update', async () => {
  stub(body([row({ verdict: 'current' }), row({ service: 'b', verdict: 'pinned' })]));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Up to date');
  expect(screen.queryByRole('button', { name: 'Plan update' })).toBeNull();
  expect(screen.queryByLabelText('Confirm update project')).toBeNull();
});
it('plans the available updates after typed confirmation and reloads the plan panel', async () => {
  document.cookie = 'ky_csrf=csrf';
  const onPlanned = vi.fn();
  const fetcher = stub(body([row({ service: 'web', verdict: 'update_available' }), row({ service: 'db', verdict: 'current' }), row({ service: 'cache', verdict: 'update_available' })]), new Response(JSON.stringify({ id: 'd1' }), { status: 201 }));
  render(<ApplicationUpdates {...props} onPlanned={onPlanned} />);
  openPanel();
  await screen.findByText('Up to date');
  const button = screen.getByRole('button', { name: 'Plan update' }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  planUpdate();
  await screen.findByText('Plan created; review it in the deployment plan panel.');
  const post = fetcher.mock.calls.find((c) => c[1]?.method === 'POST');
  expect(post?.[0]).toBe('/app/deployments');
  expect(new Headers(post?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ instance_id: 'i', mapping_version: 2, revision: 4, confirm: 'shop', update: ['web', 'cache'] });
  expect(onPlanned).toHaveBeenCalledTimes(1);
  expect((screen.getByLabelText('Confirm update project') as HTMLInputElement).value).toBe('');
});
it.each([
  ['update_not_mapped', 'Map the services first.'],
  ['registry_not_configured', 'No registry entry for this host and anonymous pulls are off.'],
  ['registry_unauthorized', 'The registry refused the credentials.'],
  ['registry_not_found', 'The image was not found in the registry.'],
  ['registry_rate_limited', 'The registry rate limit was reached; try later.'],
  ['registry_private_destination', 'The registry is on a private address this organization may not reach.'],
  ['registry_unavailable', 'The registry could not be reached.'],
])('maps the %s blocker to fixed text', async (blocker, text) => {
  const onPlanned = vi.fn();
  stub(body([row({ verdict: 'update_available' })]), new Response(JSON.stringify({ error: 'secret-canary', code: 'preflight_blocked', blockers: [blocker, 'made_up_canary'] }), { status: 409 }));
  render(<ApplicationUpdates {...props} onPlanned={onPlanned} />);
  openPanel();
  await screen.findByText('Update available');
  planUpdate();
  await screen.findByText(text);
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('made_up_canary');
  expect(document.body.textContent).not.toContain(blocker);
  expect(onPlanned).not.toHaveBeenCalled();
});
it('maps 429 to fixed text', async () => {
  stub(body([row({ verdict: 'update_available' })]), new Response(JSON.stringify({ error: 'secret-canary', code: 'too_many_checks' }), { status: 429 }));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Update available');
  planUpdate();
  await screen.findByText('Too many registry checks are running; try again in a moment.');
  expect(document.body.textContent).not.toContain('secret-canary');
});
it.each([
  ['adoption_changed', 'Adoption changed. Refresh applications before planning.'],
  ['deployment_in_progress', 'A deployment is being applied; wait for it to finish.'],
  ['made_up_canary', 'The plan was refused or its outcome is unknown. Refresh before trying again.'],
])('maps the %s conflict to fixed text', async (code, text) => {
  stub(body([row({ verdict: 'update_available' })]), new Response(JSON.stringify({ error: 'secret-canary', code }), { status: 409 }));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Update available');
  planUpdate();
  await screen.findByText(text);
  expect(document.body.textContent).not.toContain('secret-canary');
});
it('hides an unknown refusal body', async () => {
  stub(body([row({ verdict: 'update_available' })]), new Response(JSON.stringify({ error: 'secret-canary', code: 'made_up_canary' }), { status: 500 }));
  render(<ApplicationUpdates {...props} />);
  openPanel();
  await screen.findByText('Update available');
  planUpdate();
  await screen.findByText('The plan was refused or its outcome is unknown. Refresh before trying again.');
  expect(document.body.textContent).not.toContain('secret-canary');
});
