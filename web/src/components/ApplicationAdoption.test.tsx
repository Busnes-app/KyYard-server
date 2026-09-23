import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationAdoption } from './ApplicationAdoption';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
it('requires typed project confirmation and posts the reviewed digest with CSRF', async () => {
  document.cookie = 'ky_csrf=csrf';
  const changed = vi.fn();
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf');
      expect(JSON.parse(String(init.body))).toEqual({ endpoint_id: 'host', project: 'shop', digest: 'reviewed', confirm: 'shop' });
      return json({}, 201);
    }
    if (url.includes('/adoption?')) return json({ application_name: 'App', endpoint_name: 'Docker', project: 'shop', revision: 1, digest: 'reviewed', containers: [{ id: 'immutable', name: 'shop-web', image_id: 'sha256:image' }] });
    if (url.endsWith('/inventory')) return json({ snapshot: { containers: [{ compose_project: 'shop' }] } });
    return json([{ id: 'host', name: 'Docker', state: 'active' }]);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" onChanged={changed} />);
  fireEvent.change(await screen.findByLabelText('Docker host'), { target: { value: 'host' } });
  fireEvent.change(await screen.findByLabelText('Compose project'), { target: { value: 'shop' } });
  const adopt = await screen.findByRole('button', { name: 'Adopt reviewed containers' });
  expect(adopt.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Type the project name to confirm'), { target: { value: 'shop' } });
  fireEvent.click(adopt);
  await vi.waitFor(() => expect(changed).toHaveBeenCalledOnce());
});
it('names the application and host when releasing an exact instance', async () => {
  const confirm = vi.fn(() => true);
  vi.stubGlobal('confirm', confirm);
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'DELETE') { expect(JSON.parse(String(init.body))).toEqual({ instance_id: 'instance', confirm: 'shop' }); return new Response(null, { status: 204 }); }
    return json([]);
  }));
  const changed = vi.fn();
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" instance={{ id: 'instance', application_id: 'app', endpoint_id: 'host', endpoint_name: 'Docker host', project: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 0, container_count: 0, containers: [] }} onChanged={changed} />);
  fireEvent.click(screen.getByRole('button', { name: 'Release adoption' }));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Docker host'));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('App'));
  await vi.waitFor(() => expect(changed).toHaveBeenCalledOnce());
});
it('shows fixed text for a live plan instead of the server body when release is refused', async () => {
  vi.stubGlobal('confirm', vi.fn(() => true));
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'DELETE') return json({ error: 'secret-canary', code: 'deployment_planned' }, 409);
    return json([]);
  }));
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" instance={{ id: 'instance', application_id: 'app', endpoint_id: 'host', endpoint_name: 'Docker host', project: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 0, container_count: 0, containers: [] }} onChanged={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Release adoption' }));
  await screen.findByText(/deployment plan for this instance is still valid/);
  expect(document.body.textContent).not.toContain('secret-canary');
});
const adopted = { id: 'instance', application_id: 'app', endpoint_id: 'host', endpoint_name: 'Docker host', project: 'shop', revision: 1, current_revision: 1, previous_revision: 0, mapping_version: 0, container_count: 2, containers: [] };
it('removes an adopted application only after typing the project', async () => {
  document.cookie = 'ky_csrf=csrf';
  const changed = vi.fn();
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      expect(url).toBe('/applications/app/removal');
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf');
      expect(JSON.parse(String(init.body))).toEqual({ instance_id: 'instance', confirm: 'shop' });
      return json({ id: 'r1' }, 202);
    }
    return json([]);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" instance={adopted} onChanged={changed} />);
  expect(document.body.textContent).toContain('Stops and removes the 2 adopted containers of shop on Docker host. Named volumes, images and the saved revisions are kept; the application is marked removed and can be discarded later. Nothing rolls back.');
  const remove = screen.getByRole('button', { name: 'Remove application' });
  expect(remove.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Confirm removal project'), { target: { value: 'shop' } });
  fireEvent.click(remove);
  await vi.waitFor(() => expect(changed).toHaveBeenCalledOnce());
});
it.each([
  [501, {}, 'Upgrade the host agent to enable application removal.'],
  [409, { code: 'deployment_in_progress' }, 'A deployment is planned or running for this instance; wait or let the plan expire.'],
])('maps a %i removal refusal to fixed text', async (status, extra, text) => {
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => init?.method === 'POST' ? json({ error: 'secret-canary', ...extra }, status) : json([])));
  const changed = vi.fn();
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" instance={adopted} onChanged={changed} />);
  fireEvent.change(screen.getByLabelText('Confirm removal project'), { target: { value: 'shop' } });
  fireEvent.click(screen.getByRole('button', { name: 'Remove application' }));
  await screen.findByText(text);
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(changed).not.toHaveBeenCalled();
});
it('offers removal only for an adopted instance', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json([])));
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" onChanged={vi.fn()} />);
  await screen.findByLabelText('Docker host');
  expect(screen.queryByRole('button', { name: 'Remove application' })).toBeNull();
});
