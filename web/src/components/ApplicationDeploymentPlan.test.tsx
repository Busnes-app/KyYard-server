import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const mapping = { instance_id: 'i', version: 3, mapped_revision: 2, services: [], bindings: {}, preview: { revision: 2, digest: 'd', project: 'shop', endpoint_name: 'Docker', containers: [] } };
const plan = { id: 'd1', application_id: 'app', instance_id: 'i', endpoint_id: 'host', state: 'planned', revision: 2, spec_digest: 'x', mapping_version: 1, created_by: 'u', created_at: '2026-09-22T12:00:00Z', expires_at: '2999-01-01T00:00:00Z', expired: false, detail: '', result: null, plan: { project: 'shop', services: [{ name: 'web', reference: 'nginx:1', image_id: `sha256:${'a'.repeat(64)}`, image_digest: '', container_id: 'b'.repeat(64), replaces: { container_id: 'b'.repeat(64), image_id: `sha256:${'c'.repeat(64)}`, created_unix: 1 }, restart: 'always', ports: [], secret_refs: ['TOKEN'] }] } };
const props = { base: '/app', instanceID: 'i' };
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
  expect(screen.getByText(/not executed/i)).toBeTruthy();
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
  expect(screen.getAllByRole('row').length).toBe(2); // header + one service
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
const refused = { ...plan, state: 'denied', detail: 'service web, step precondition: the container has mounts', result: { steps: [{ service: 'web', step: 'precondition', outcome: 'denied', detail: 'the container has mounts the definition does not describe' }, { service: 'web', step: 'image', outcome: 'skipped', detail: '' }], services: [] } };

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
  // (the setInterval registration and its state updates) to flush on each advance.
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
