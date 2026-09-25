import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
import { messages } from './ApplicationPreflight';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const mapping = { instance_id: 'i', version: 3, mapped_revision: 2, services: [], bindings: {}, preview: { revision: 2, digest: 'd', project: 'shop', endpoint_name: 'Docker', containers: [] } };
const plan = { id: 'd1', application_id: 'app', instance_id: 'i', endpoint_id: 'host', state: 'planned', revision: 2, spec_digest: 'x', mapping_version: 1, created_by: 'u', created_at: '2026-09-22T12:00:00Z', expires_at: '2999-01-01T00:00:00Z', expired: false, detail: '', result: null, plan: { project: 'shop', services: [{ name: 'web', reference: 'nginx:1', image_id: `sha256:${'a'.repeat(64)}`, image_digest: '', container_id: 'b'.repeat(64), replaces: { container_id: 'b'.repeat(64), image_id: `sha256:${'c'.repeat(64)}`, created_unix: 1 }, restart: 'always', ports: [], secret_refs: ['TOKEN'] }] } };
const instance = { id: 'i', application_id: 'app', endpoint_id: 'host', endpoint_name: 'Docker', project: 'shop', revision: 2, current_revision: 2, previous_revision: 0, mapping_version: 3, container_count: 1, containers: [] };
const props = { base: '/app', instanceID: 'i', latestRevision: 2, instance };
function stubFetch(deploymentsBody: unknown) {
  return vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify(plan));
    return new Response(JSON.stringify(deploymentsBody));
  });
}
it('loads the current plan on open', async () => {
  vi.stubGlobal('fetch', stubFetch([plan]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(screen.getByText(/Planning executes nothing/)).toBeTruthy();
  expect(document.body.textContent).not.toContain('inert');
});
it('plans on explicit confirmation and posts the observed bindings', async () => {
  const fetcher = stubFetch([]);
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  const post = fetcher.mock.calls.find(c => (c[1] as RequestInit | undefined)?.method === 'POST');
  expect(post?.[0]).toBe('/app/deployments');
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ instance_id: 'i', mapping_version: 3, revision: 2, confirm: 'shop' });
});
it('renders blockers as fixed text and hides error bodies', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify({ error: 'secret-canary', code: 'preflight_blocked', blockers: ['image_not_reported', 'made_up_blocker'] }), { status: 409 });
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByRole('alert');
  expect(document.body.textContent).toContain('not reported');
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('made_up_blocker');
});
it('shows an expired plan as expired and hides a plan for another instance', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, expired: true }, { ...plan, id: 'd2', instance_id: 'other' }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/expired/i);
  expect(screen.getAllByRole('table')[0].querySelectorAll('tr').length).toBe(2); // header + one service
  expect(screen.getAllByRole('button', { name: 'Show steps' }).length).toBe(1); // history excludes the other instance
});
it('shows adoption changed and no form when the mapping names a different instance', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify({ ...mapping, instance_id: 'other' }));
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByRole('alert');
  expect(document.body.textContent).toContain('Adoption changed');
  expect(screen.queryByLabelText('Confirm plan project')).toBeNull();
});
const applying = { ...plan, state: 'applying', detail: '', result: null };
const settled = { ...plan, state: 'succeeded', detail: '', result: { steps: [{ service: 'web', step: 'create', outcome: 'succeeded', detail: '' }], services: [{ service: 'web', container_id: 'e'.repeat(64), image_id: `sha256:${'a'.repeat(64)}`, created_unix: 1800000000 }] } };
const refused = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'unsupported', detail: 'privileged' }, { service: 'web', step: 'image', outcome: 'skipped', detail: '' }], services: [] } };

it('applies on typed confirmation and polls until settled', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const fetcher = vi.fn(async (url: string, _init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (String(url).endsWith('/apply')) return new Response(JSON.stringify(applying), { status: 202 });
    reads++;
    return new Response(JSON.stringify([reads < 3 ? (reads === 1 ? plan : applying) : settled]));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await vi.waitFor(() => screen.getByRole('button', { name: 'Apply deployment' }));
  fireEvent.change(screen.getByLabelText('Confirm apply project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply deployment' }));
  const post = fetcher.mock.calls.find(c => String(c[0]).endsWith('/apply'));
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ confirm: 'shop' });
  // vi.waitFor's own polling misbehaves under fake timers here; act() forces passive effects
  // (the setInterval registration and its state updates) to flush on each advance. The interval
  // is registered mid-flight of the first advance (after the apply POST resolves), so its first
  // tick lands in the *second* advance; three advances cover two actual polls (reads 2 and 3).
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(document.body.textContent).toMatch(/succeeded/i);
  const before = fetcher.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); });
  expect(fetcher.mock.calls.length).toBe(before); // polling stopped on a terminal state
  expect(screen.getByText('e'.repeat(64))).toBeTruthy();
  vi.useRealTimers();
});
it('explains a refused precondition with fixed text and hides server detail', async () => {
  vi.stubGlobal('fetch', stubFetch([refused]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/configuration the definition does not describe/i);
  expect(screen.getByText(/skipped/i)).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Apply deployment' })).toBeNull();
});
it('shows the plan even when the mapping read fails', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => String(url).endsWith('/mapping') ? new Response('{"error":"secret-canary"}', { status: 409 }) : new Response(JSON.stringify([plan]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(screen.queryByRole('button', { name: 'Plan deployment' })).toBeNull();
});
it('keeps the panel mounted across polls instead of flashing loading', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const fetcher = vi.fn(async (url: string) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (String(url).endsWith('/apply')) return new Response(JSON.stringify(applying), { status: 202 });
    reads++;
    return new Response(JSON.stringify([reads === 1 ? plan : applying]));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await vi.waitFor(() => screen.getByRole('button', { name: 'Apply deployment' }));
  fireEvent.change(screen.getByLabelText('Confirm apply project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply deployment' }));
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(document.body.textContent).toMatch(/State: applying/);
  expect(document.body.textContent).not.toMatch(/Loading/);
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(document.body.textContent).toMatch(/State: applying/);
  expect(document.body.textContent).not.toMatch(/Loading/);
  vi.useRealTimers();
});
it('pauses status updates after three consecutive poll failures and stops polling', async () => {
  vi.useFakeTimers();
  let reads = 0;
  const fetcher = vi.fn(async (url: string) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (String(url).endsWith('/apply')) return new Response(JSON.stringify(applying), { status: 202 });
    reads++;
    if (reads === 1) return new Response(JSON.stringify([plan]));
    return new Response('server error', { status: 500 });
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await vi.waitFor(() => screen.getByRole('button', { name: 'Apply deployment' }));
  fireEvent.change(screen.getByLabelText('Confirm apply project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply deployment' }));
  // The interval is registered mid-flight of the first advance, so its first tick lands in the
  // second advance; four advances cover three actual failed polls.
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); }); // registers the interval
  expect(document.body.textContent).not.toContain('Status updates paused');
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); }); // failure 1
  expect(document.body.textContent).not.toContain('Status updates paused');
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); }); // failure 2
  expect(document.body.textContent).not.toContain('Status updates paused');
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); }); // failure 3: pause
  expect(document.body.textContent).toContain('Status updates paused; refresh to continue.');
  const before = fetcher.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); });
  expect(fetcher.mock.calls.length).toBe(before); // no further polling once paused
  vi.useRealTimers();
});
it('renders step codes from the table and anything else as inert text', async () => {
  const mixed = { ...plan, state: 'failed', detail: 'unexpected raw server text', result: { code: 'step_failed', steps: [
    { service: 'web', step: 'precondition', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'start', outcome: 'failed', code: 'identity_unverified', detail: 'c'.repeat(64) },
    { service: 'web', step: 'stop', outcome: 'failed', code: 'runtime_status', detail: '500' },
    { service: 'web', step: 'remove', outcome: 'failed', code: 'runtime_status', detail: 'abc' },
    { service: 'web', step: 'create', outcome: 'failed', code: '<img src=x onerror=alert(1)>', detail: 'secret-canary' },
  ], services: [] } };
  vi.stubGlobal('fetch', stubFetch([mixed]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`The container started but its identity could not be verified (container ${'c'.repeat(64)}).`);
  expect(screen.getByText('The runtime refused with status 500.')).toBeTruthy();
  expect(screen.getByText('The runtime refused.')).toBeTruthy();
  expect(screen.getByText('unrecognised outcome `<img src=x onerror=alert(1)>`')).toBeTruthy();
  expect(screen.getByText('A step did not succeed; the steps say which.')).toBeTruthy();
  expect(document.querySelector('img')).toBeNull();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('unexpected raw server text');
});
it('explains an abandoned unknown row with fixed text', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'unknown', detail: 'the connection ended before a result arrived', result: null }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host may or may not have acted. Inspect it before planning again.');
  expect(screen.getByText('the connection ended before a result arrived')).toBeTruthy();
});
it('explains a deployment that was never sent', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'failed', detail: 'the endpoint disconnected before the deployment was sent', result: null }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The deployment was not sent to the host.');
  expect(document.body.textContent).not.toContain('.kyyard-prev');
});
it('keys a refused precondition on its step code', async () => {
  const gone = { ...refused, result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'container_missing', detail: '' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([gone]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The mapped container no longer exists on the host; refresh the inventory and plan again.');
  expect(screen.getByText('The container no longer exists.')).toBeTruthy();
});
it('stops polling three minutes past the deadline and says so', async () => {
  vi.useFakeTimers();
  const running = { ...applying, deadline: new Date(Date.now() + 60_000).toISOString() };
  const fetcher = vi.fn(async (url: string) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    return new Response(JSON.stringify([running]));
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await vi.waitFor(() => screen.getByText(/State: applying/));
  // Three minutes in: still polling. Past four (deadline + 3 minutes): paused.
  for (let i = 0; i < 9; i++) await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
  expect(document.body.textContent).not.toContain('Status updates paused');
  for (let i = 0; i < 4; i++) await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
  expect(document.body.textContent).toContain('Status updates paused; refresh to continue.');
  const before = fetcher.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
  expect(fetcher.mock.calls.length).toBe(before);
  vi.useRealTimers();
});
it('plans a prior revision with the reapply warning', async () => {
  const fetcher = stubFetch([]);
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  expect(screen.getByText('Current revision 2 · previous none')).toBeTruthy();
  const select = screen.getByLabelText('Revision to plan');
  expect((select as HTMLSelectElement).value).toBe('2');
  expect(document.body.textContent).not.toContain('uses its own saved environment values');
  fireEvent.change(select, { target: { value: '1' } });
  expect(screen.getByText('Revision 1 uses its own saved environment values. Data written since is not reversed. The service mapping was reviewed against revision 2.')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await vi.waitFor(() => expect(fetcher.mock.calls.some(c => (c[1] as RequestInit | undefined)?.method === 'POST')).toBe(true));
  const post = fetcher.mock.calls.find(c => (c[1] as RequestInit | undefined)?.method === 'POST');
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ instance_id: 'i', mapping_version: 3, revision: 1, confirm: 'shop' });
});
const removal = { ...plan, id: 'r1', kind: 'remove', state: 'denied', revision: 2, applied_by: 'admin-user', applied_at: '2026-09-23T10:00:00Z', settled_at: '2026-09-23T10:01:00Z', detail: '', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'identity_mismatch', detail: '' }], services: [] }, plan: { project: 'shop', services: [], containers: [{ service: 'web', container_id: 'f'.repeat(64), image_id: `sha256:${'c'.repeat(64)}`, created_unix: 1, name: 'shop-web-1' }] } };
const applied = { ...settled, id: 'a1', kind: 'apply', revision: 1, applied_by: 'operator-user', applied_at: '2026-09-22T10:00:00Z', settled_at: '2026-09-22T10:01:00Z' };
it('lists deployment history newest first and expands steps', async () => {
  vi.stubGlobal('fetch', stubFetch([removal, applied]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByRole('heading', { name: 'Deployment history' });
  const rows = screen.getAllByRole('row').filter(r => r.textContent?.includes('-user'));
  expect(rows.length).toBe(2);
  expect(rows[0].textContent).toContain('Removal');
  expect(rows[0].textContent).toContain('denied');
  expect(rows[1].textContent).toContain('Apply');
  expect(rows[1].textContent).toContain('succeeded');
  const toggles = screen.getAllByRole('button', { name: 'Show steps' });
  fireEvent.click(toggles[1]);
  expect(screen.getByText('e'.repeat(64))).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Hide steps' })).toBeTruthy();
});
it('renders a removal plan as its container list', async () => {
  vi.stubGlobal('fetch', stubFetch([removal]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('f'.repeat(64));
  expect(screen.getByText('shop-web-1')).toBeTruthy();
  expect(screen.queryByRole('columnheader', { name: 'Pinned image' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Apply deployment' })).toBeNull();
});
it('advises on a denied removal precondition without asking to plan again', async () => {
  vi.stubGlobal('fetch', stubFetch([removal]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  const alerts = await screen.findAllByText('A container of this application is not the one recorded; refresh the inventory and, if it was recreated outside KyYard, release and adopt it again.');
  expect(alerts.length).toBeGreaterThan(0);
  expect(document.body.textContent).not.toContain('plan again');
});
it('explains a prior revision whose services differ from the mapping', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['revision_services_differ'] }), { status: 409 });
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Revision to plan'), { target: { value: '1' } });
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByText("The chosen revision's services differ from the mapped ones. Map against the latest definition or choose another revision.");
});

it('shows the pinned registry digest for a pulled service', async () => {
  const digest = `sha256:${'abcdef012345'}${'9'.repeat(52)}`;
  const pulled = { ...plan, plan: { project: 'shop', services: [{ ...plan.plan.services[0], pull_reference: `registry-1.docker.io/library/nginx@${digest}`, pull_digest: digest }, { ...plan.plan.services[0], name: 'db' }] } };
  vi.stubGlobal('fetch', stubFetch([pulled]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('pulls abcdef012345');
  expect(screen.getAllByText(/^pulls /).length).toBe(1);
  expect(document.body.textContent).not.toContain('9'.repeat(52));
  // The pulled row's pinned image is the digest, not the image the host runs now.
  const pinned = document.querySelectorAll('td[data-label="Pinned image"]');
  expect(pinned[0].textContent).not.toContain(plan.plan.services[0].image_id);
  expect(pinned[1].textContent).toContain(plan.plan.services[0].image_id);
});
it('re-reads the plan when refreshKey changes', async () => {
  const fetcher = stubFetch([]);
  vi.stubGlobal('fetch', fetcher);
  const { rerender } = render(<ApplicationDeploymentPlan {...props} refreshKey={0} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  const reads = fetcher.mock.calls.filter(c => String(c[0]).endsWith('/deployments')).length;
  fetcher.mockImplementation(async (url: string) => new Response(JSON.stringify(String(url).endsWith('/mapping') ? mapping : [plan])));
  rerender(<ApplicationDeploymentPlan {...props} refreshKey={1} />);
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(fetcher.mock.calls.filter(c => String(c[0]).endsWith('/deployments')).length).toBe(reads + 1);
});
it('lists each service mount and the volumes the plan ensures', async () => {
  const mounted = { ...plan, plan: { ...plan.plan, volumes: ['shop_db_data', 'shared'], services: [{ ...plan.plan.services[0], mounts: [{ kind: 'volume', source: 'shop_db_data', target: '/data' }, { kind: 'bind', source: '/srv/conf', target: '/etc/app', read_only: true }] }] } };
  vi.stubGlobal('fetch', stubFetch([mounted]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText('Volumes to ensure: shop_db_data, shared')).toBeTruthy();
  expect(screen.getByText('shop_db_data → /data')).toBeTruthy();
  expect(screen.getByText('/srv/conf → /etc/app').parentElement?.querySelector('.badge')?.textContent).toBe('ro');
  expect(screen.queryByText('Will be dropped by the recreate:')).toBeNull();
});
it('shows the mounts the recreate drops for approval', async () => {
  const dropping = { ...plan, plan: { ...plan.plan, services: [{ ...plan.plan.services[0], mounts: [], dropped_mounts: [{ kind: 'bind', source: '/srv/old', target: '/old' }, { kind: 'volume', source: 'shop_cache', target: '/cache' }] }] } };
  vi.stubGlobal('fetch', stubFetch([dropping]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText('Will be dropped by the recreate:')).toBeTruthy();
  expect(screen.getByText('/srv/old → /old')).toBeTruthy();
  expect(screen.getByText('shop_cache → /cache')).toBeTruthy();
});
it('names the services a live inspection refused and hides anything unrecognized', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (String(url).endsWith('/mapping')) return new Response(JSON.stringify(mapping));
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['configuration_unsupported', 'inspection_unavailable'], services: [
      { name: 'web', blockers: ['configuration_unsupported'], unsupported: ['privileged', 'devices', 'secret-canary'] },
      { name: 'db', blockers: ['inspection_unavailable'] },
      { name: 'Bad Name', blockers: ['configuration_unsupported'], unsupported: ['privileged'] },
    ] }), { status: 409 });
    return new Response('[]');
  }));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByRole('alert');
  expect(screen.getByText(messages.configuration_unsupported)).toBeTruthy();
  expect(screen.getByText(messages.inspection_unavailable)).toBeTruthy();
  expect(screen.getByText('web: runs privileged, maps host devices')).toBeTruthy();
  expect(screen.getByText('db: no live inspection answered')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('Bad Name');
});
it('explains a recheck denial like a precondition', async () => {
  const drifted = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [
    { service: 'web', step: 'precondition', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'image', outcome: 'succeeded', detail: '' },
    { service: 'web', step: 'recheck', outcome: 'denied', code: 'configuration_drift', detail: '' },
    { service: 'web', step: 'rename', outcome: 'skipped', detail: '' },
  ], services: [] } };
  vi.stubGlobal('fetch', stubFetch([drifted]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/changed on the host while the deployment prepared/);
  expect(screen.getByText('The container changed after the precondition.')).toBeTruthy();
});
it('explains a deployment refused for clock skew', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'failed', detail: '', result: { code: 'clock_skew', steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.');
  expect(screen.getByText('The host clock differs from the server by more than five minutes; nothing ran.')).toBeTruthy();
});
it('shows the restart outcome with the inspect-the-host guidance', async () => {
  vi.stubGlobal('fetch', stubFetch([{ ...plan, state: 'unknown', detail: '', result: { code: 'restarted', steps: [], services: [] } }]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The host may or may not have acted. Inspect it before planning again.');
  expect(screen.getByText('The agent restarted after replacement began; inspect the host.')).toBeTruthy();
});
it('names unsupported configuration from its codes and drops unknown ones', async () => {
  const refusedCodes = { ...plan, state: 'denied', detail: '', result: { code: 'step_failed', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'unsupported', detail: 'privileged,devices,made_up' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([refusedCodes]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('The container has configuration the definition cannot express: runs privileged, maps host devices.');
  expect(screen.getByText(/configuration the definition does not describe/i)).toBeTruthy();
  expect(document.body.textContent).not.toContain('made_up');
});
it('asks for an agent upgrade on a legacy outcome and hides its old sentence', async () => {
  const older = { ...plan, state: 'denied', detail: 'service web, step precondition: the container no longer exists', result: { code: 'legacy', steps: [{ service: 'web', step: 'precondition', outcome: 'denied', code: 'legacy', detail: '' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([older]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect((await screen.findAllByText('The agent did not classify this outcome; upgrade the agent.')).length).toBe(2);
  expect(document.body.textContent).not.toContain('no longer exists');
  expect(document.body.textContent).not.toContain('configuration the definition does not describe');
});
it.each([
  ['planned', { ...plan, correlation_id: '0123456789abcdef0123456789abcdef' }],
  ['settled', { ...settled, correlation_id: '0123456789abcdef0123456789abcdef' }],
])('shows the %s deployment correlation ID once', async (_state, row) => {
  vi.stubGlobal('fetch', stubFetch([row]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect((await screen.findAllByText('0123456789abcdef0123456789abcdef')).length).toBe(1);
  expect(screen.getByText(/search the audit log for it/)).toBeTruthy();
});
