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
  render(<ApplicationAdoption base="/applications/app" applicationName="App" org="a" env="env" instance={{ id: 'instance', application_id: 'app', endpoint_id: 'host', endpoint_name: 'Docker host', project: 'shop', revision: 1, container_count: 0, containers: [] }} onChanged={changed} />);
  fireEvent.click(screen.getByRole('button', { name: 'Release adoption' }));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Docker host'));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('App'));
  await vi.waitFor(() => expect(changed).toHaveBeenCalledOnce());
});
