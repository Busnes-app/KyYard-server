import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const plan = { id: 'd1', application_id: 'app', instance_id: 'i', endpoint_id: 'host', state: 'planned', revision: 2, spec_digest: 'x', mapping_version: 1, created_by: 'u', created_at: '2026-09-22T12:00:00Z', expires_at: '2999-01-01T00:00:00Z', expired: false, plan: { project: 'shop', services: [{ name: 'web', reference: 'nginx:1', image_id: `sha256:${'a'.repeat(64)}`, image_digest: '', container_id: 'b'.repeat(64), replaces: { container_id: 'b'.repeat(64), image_id: `sha256:${'c'.repeat(64)}`, created_unix: 1 }, restart: 'always', ports: [], secret_refs: ['TOKEN'] }] } };
const props = { base: '/app', instanceID: 'i', mappingVersion: 1, revision: 2, project: 'shop' };
it('loads the current plan on open and never offers apply', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify([plan]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  expect(screen.getByText(/not executed/i)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /apply|deploy now|execute/i })).toBeNull();
});
it('plans on explicit confirmation and posts the observed bindings', async () => {
  const fetcher = vi.fn(async (_url: string, init?: RequestInit) => new Response(JSON.stringify(init?.method === 'POST' ? plan : [])));
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText('No plan for this instance.');
  fireEvent.change(screen.getByLabelText('Confirm plan project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Plan deployment' }));
  await screen.findByText(`sha256:${'a'.repeat(64)}`);
  const post = fetcher.mock.calls.find(c => (c[1] as RequestInit | undefined)?.method === 'POST');
  expect(post?.[0]).toBe('/app/deployments');
  expect(JSON.parse(String((post?.[1] as RequestInit).body))).toEqual({ instance_id: 'i', mapping_version: 1, revision: 2, confirm: 'shop' });
});
it('renders blockers as fixed text and hides error bodies', async () => {
  vi.stubGlobal('fetch', vi.fn(async (_: string, init?: RequestInit) => init?.method === 'POST' ? new Response(JSON.stringify({ error: 'secret-canary', code: 'preflight_blocked', blockers: ['image_not_reported', 'made_up_blocker'] }), { status: 409 }) : new Response('[]')));
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
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify([{ ...plan, expired: true }, { ...plan, id: 'd2', instance_id: 'other' }]))));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  await screen.findByText(/expired/i);
  expect(screen.getAllByRole('row').length).toBe(2); // header + one service
});
