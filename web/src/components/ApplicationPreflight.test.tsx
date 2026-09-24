import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationPreflight } from './ApplicationPreflight';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const data = { instance_id: 'i', endpoint_name: 'Docker', revision: 2, mapping_version: 1, received_at: '2026-09-18T12:00:00Z', executable: false, blockers: ['mapping_requires_review'], services: Array.from({ length: 26 }, (_, i) => ({ name: `service-${i}`, reference: 'nginx:1', image_id: '', container_id: 'a'.repeat(64), blockers: ['image_not_reported'] })) };
it('loads on demand, pages results and never offers execution', async () => {
  const fetcher = vi.fn(async () => new Response(JSON.stringify(data)));
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  await screen.findByRole('heading', { name: 'Deployment preflight' });
  expect(screen.getByText('Review and save service mapping for the latest definition.')).toBeTruthy();
  expect(screen.queryByText(/eady to plan/)).toBeNull();
  expect(document.body.textContent).not.toContain('not available yet');
  expect(screen.getAllByRole('row')).toHaveLength(26);
  expect(screen.queryByText('service-25')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
  expect(screen.getByText('service-25')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /apply|execute|deploy$/i })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Refresh preflight' }));
  expect(fetcher).toHaveBeenCalledWith('/app/preflight');
});
it('says ready to plan when no blocker remains', async () => {
  const ready = { ...data, executable: true, blockers: [], services: [{ ...data.services[0], image_id: `sha256:${'c'.repeat(64)}`, blockers: [] }] };
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(ready))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  await screen.findByRole('heading', { name: 'Deployment preflight' });
  expect(screen.getByText('Ready to plan: every service maps to an adopted container and its image is known on the host.')).toBeTruthy();
  expect(document.body.textContent).not.toContain('runtime verification');
});
it('says not ready when only service findings remain', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...data, blockers: [], services: data.services.slice(0, 1) }))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  expect(await screen.findByText('Not ready to plan.')).toBeTruthy();
  expect(screen.getByText(data.services[0]!.name)).toBeTruthy();
  expect(screen.queryByText(/Ready to plan/)).toBeNull();
});
it('hides observations after adoption replacement and redacts error bodies', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...data, instance_id: 'replacement' }))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Adoption changed');
  expect(screen.queryByRole('table')).toBeNull();
  cleanup();
  vi.stubGlobal('fetch', vi.fn(async () => new Response('secret-canary', { status: 409 })));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  await screen.findByRole('alert');
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.queryByRole('table')).toBeNull();
});

const target = { container_id: 'a'.repeat(64), image_id: `sha256:${'b'.repeat(64)}`, created_unix: 1789732000 };
const inspection = { target, observed_at: '2026-09-18T12:00:00Z', state: 'running', image_platform: { os: 'linux', architecture: 'amd64' }, restart_policy: 'on-failure', restart_retries: 3, ports: [{ host: 8080, container: 80, protocol: 'tcp', host_ip: '127.0.0.1' }], mounts: { bind: 1, volume: 2, tmpfs: 0, other: 0, read_only: 1 }, network_mode: 'bridge', network_count: 1, privileged: false, read_only_rootfs: true, auto_remove: false, configuration_verified: false };
const inspectable = { ...data, endpoint_id: 'host', services: [{ ...data.services[0], inspection_target: target }] };
function modal() { Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.open = true; } }); }
async function openInspection() {
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Inspect service-0' }));
}
it('inspects only on request, displays selected facts and discards results on close', async () => {
  modal();
  const fetcher = vi.fn(async (url: string) => new Response(JSON.stringify(url.endsWith('/preflight') ? inspectable : { ...inspection, environment: 'secret-canary' })));
  vi.stubGlobal('fetch', fetcher);
  await openInspection();
  await screen.findByText('linux/amd64');
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls[1]?.[0]).toBe(`/api/organizations/org/endpoints/host/containers/${target.container_id}/inspection`);
  expect(screen.getByText('8080 → 80/tcp')).toBeTruthy();
  expect(screen.getByText('127.0.0.1')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.queryByRole('button', { name: /apply|execute|deploy$/i })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Close inspection' }));
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.queryByText('linux/amd64')).toBeNull();
});
it.each([
  { ...inspection, target: { ...target, image_id: `sha256:${'c'.repeat(64)}` } },
  { ...inspection, target: { ...target, container_id: 'd'.repeat(64) } },
  { ...inspection, target: { ...target, created_unix: target.created_unix + 1 } },
  { ...inspection, configuration_verified: true },
  { ...inspection, mounts: null },
  { ...inspection, state: 'secret-canary' },
])('refuses mismatched or malformed observations', async (response) => {
  modal();
  vi.stubGlobal('fetch', vi.fn(async (url: string) => new Response(JSON.stringify(url.endsWith('/preflight') ? inspectable : response))));
  await openInspection();
  expect((await screen.findByRole('alert')).textContent).toContain('did not match');
  expect(screen.queryByText('linux/amd64')).toBeNull();
  expect(document.body.textContent).not.toContain('secret-canary');
});
it.each([403, 409, 429, 501, 504])('shows fixed inspection failure for HTTP %s without retrying', async (status) => {
  modal();
  const fetcher = vi.fn(async (url: string) => url.endsWith('/preflight') ? new Response(JSON.stringify(inspectable)) : new Response('secret-canary', { status }));
  vi.stubGlobal('fetch', fetcher);
  await openInspection();
  await screen.findByRole('alert');
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(fetcher).toHaveBeenCalledTimes(2);
});
it('aborts an outstanding request on dismissal and ignores its late response', async () => {
  modal();
  let requestSignal: AbortSignal | null | undefined;
  let finish: (response: Response) => void = () => { throw new Error('inspection not requested'); };
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    if (url.endsWith('/preflight')) return Promise.resolve(new Response(JSON.stringify(inspectable)));
    requestSignal = init?.signal;
    return new Promise<Response>(resolve => { finish = resolve; });
  }));
  await openInspection();
  expect(requestSignal?.aborted).toBe(false);
  fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
  expect(requestSignal?.aborted).toBe(true);
  finish(new Response(JSON.stringify(inspection)));
  expect(screen.queryByRole('dialog')).toBeNull();
});
it('shows mounts, the volume blocker texts and the mounts a recreate drops', async () => {
  const mounted = { ...data, services: [
    { ...data.services[0], name: 'db', blockers: ['bind_mount_new'], mounts: [{ kind: 'volume', source: 'shop_db_data', target: '/var/lib/postgresql/data' }, { kind: 'bind', source: '/srv/new', target: '/new', read_only: true }], dropped_mounts: [{ kind: 'bind', source: '/srv/old', target: '/old' }, { kind: 'volume', source: 'shop_cache', target: '/cache' }], dropped_binds: [{ kind: 'bind', source: '/srv/old', target: '/old' }] },
    { ...data.services[1], name: 'web', blockers: ['mounts_unreported'], mounts: [], dropped_mounts: [], dropped_binds: [] },
  ] };
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(mounted))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  await screen.findByRole('heading', { name: 'Deployment preflight' });
  expect(screen.getByText('This revision adds a host path the running container does not have; KyYard never introduces bind mounts. Mount it by hand first, or drop it from the definition.')).toBeTruthy();
  expect(screen.getByText("The host has not reported this container's mounts; wait for the agent to reconnect or re-adopt.")).toBeTruthy();
  expect(screen.getByText('shop_db_data → /var/lib/postgresql/data')).toBeTruthy();
  expect(screen.getByText('/srv/new → /new').parentElement?.querySelector('.badge')?.textContent).toBe('ro');
  expect(screen.getAllByText('Will be dropped by the recreate:')).toHaveLength(1);
  expect(screen.getByText('/srv/old → /old')).toBeTruthy();
  expect(screen.getByText('shop_cache → /cache')).toBeTruthy();
});
it('shows unsupported mounts and the missing-volume text', async () => {
  const unsupported = { ...data, services: [{ ...data.services[0], name: 'db', blockers: ['mount_unsupported', 'volume_missing'], mounts: [], dropped_mounts: [], unsupported_mounts: [{ kind: 'other', source: '', target: '/scratch' }] }] };
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(unsupported))));
  render(<ApplicationPreflight org="org" base="/app" instanceID="i" />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment preflight' }));
  await screen.findByRole('heading', { name: 'Deployment preflight' });
  expect(screen.getByText('This container has mounts KyYard cannot recreate (anonymous volumes or unsupported mount types). Recreate it by hand with named volumes first.')).toBeTruthy();
  expect(screen.getByText('An external volume this revision names does not exist on the host. Create it there first.')).toBeTruthy();
  expect(screen.getByText('Cannot be recreated:')).toBeTruthy();
  expect(screen.getByText('→ /scratch').parentElement?.textContent).toContain('other');
});
