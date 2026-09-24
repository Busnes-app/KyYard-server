import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationUpdates } from './ApplicationUpdates';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const local = `sha256:${'1234567890ab'}${'c'.repeat(52)}`;
const remote = `sha256:${'fedcba987654'}${'d'.repeat(52)}`;
const row = (over: Record<string, string>) => ({ service: 'web', reference: 'nginx:1', local_digest: local, remote_digest: remote, verdict: 'current', detail: '', checked_at: '2026-09-23T12:00:00Z', ...over });
const body = (services: unknown[]) => ({ instance_id: 'i', mapping_version: 2, services });
const props = { base: '/app', instanceID: 'i', mappingVersion: 2 };
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
  await screen.findByText('No update check yet.');
  fireEvent.click(screen.getByRole('button', { name: 'Check for updates' }));
  await screen.findByText(text);
  expect(document.body.textContent).not.toContain('secret-canary');
});
it('hides rows cached for another instance', async () => {
  stub({ instance_id: 'other', mapping_version: 2, services: [row({})] });
  render(<ApplicationUpdates {...props} />);
  await screen.findByText('No update check yet.');
  expect(screen.queryByText('Up to date')).toBeNull();
});
