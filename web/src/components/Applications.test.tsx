import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Applications } from './Applications';
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
it('imports with CSRF, clears sensitive input and shows only saved references', async () => {
  let saved = false;
  document.cookie = 'ky_csrf=test-csrf';
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('test-csrf');
      expect(JSON.parse(String(init.body))).toEqual({ name: 'shop', compose: 'secret-canary' });
      saved = true;
      return json({}, 201);
    }
    if (url.includes('/revisions/')) return json({ digest: 'digest', spec: { services: [{ name: 'web', image: 'nginx:1', environment: { TOKEN: { secret_ref: 'ref' } } }] } });
    return json(saved ? [{ id: 'app', name: 'shop', latest_revision: 1 }] : []);
  }));
  render(<Applications org="a" env="env" />);
  fireEvent.click(screen.getByText('Import Compose draft'));
  fireEvent.change(screen.getByLabelText('Application name'), { target: { value: 'shop' } });
  fireEvent.change(screen.getByLabelText('Compose YAML (up to 64 KiB)'), { target: { value: 'secret-canary' } });
  fireEvent.click(screen.getByRole('button', { name: 'Import draft' }));
  expect(await screen.findByText('Draft imported. No containers were changed.')).toBeTruthy();
  expect(screen.getByLabelText('Compose YAML (up to 64 KiB)')).toHaveProperty('value', '');
  fireEvent.click(await screen.findByRole('button', { name: 'View configuration for shop' }));
  expect(await screen.findByText('Encrypted environment keys: TOKEN')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
});
it('does not echo diagnostics and blocks retries for an unknown write result', async () => {
  let status = 400;
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => init?.method === 'POST' ? json({ diagnostic: { line: 2, reason: 'secret-canary' } }, status) : json([])));
  render(<Applications org="a" env="env" />);
  fireEvent.click(screen.getByText('Import Compose draft'));
  fireEvent.change(screen.getByLabelText('Application name'), { target: { value: 'shop' } });
  fireEvent.change(screen.getByLabelText('Compose YAML (up to 64 KiB)'), { target: { value: 'services: {}' } });
  fireEvent.click(screen.getByRole('button', { name: 'Import draft' }));
  expect(await screen.findByText(/refused near line 2/)).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  status = 500;
  fireEvent.click(screen.getByRole('button', { name: 'Import draft' }));
  await screen.findByText(/outcome is unknown/);
  expect(screen.getByRole('button', { name: 'Import draft' }).hasAttribute('disabled')).toBe(true);
});
it('reads historical definitions without changing the head used for a new save', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url.includes('/revisions/')) return json({ digest: 'digest', spec: { services: [{ name: 'web', image: url.endsWith('/1') ? 'nginx:1' : 'nginx:2' }] } });
    if (url.includes('/instances') || url.includes('/endpoints')) return json([]);
    return json([{ id: 'app', name: 'shop', latest_revision: 2 }]);
  }));
  render(<Applications org="a" env="env" />);
  fireEvent.click(await screen.findByRole('button', { name: 'View configuration for shop' }));
  await screen.findByText(/nginx:2/);
  fireEvent.change(screen.getByLabelText('Saved revision'), { target: { value: '1' } });
  await screen.findByText(/nginx:1/);
  fireEvent.click(screen.getByRole('button', { name: 'Save new revision' }));
  expect(screen.getByRole('button', { name: 'Save revision 3' })).toBeTruthy();
});
it('labels a removed application, enables discard and offers re-adoption only', async () => {
  const removedAt = '2026-09-20T12:00:00Z';
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url.includes('/revisions/')) return json({ digest: 'digest', spec: { services: [] } });
    if (url.includes('/instances') || url.includes('/endpoints')) return json([]);
    return json([{ id: 'app', name: 'shop', latest_revision: 2, removed_at: removedAt }]);
  }));
  render(<Applications org="a" env="env" />);
  await vi.waitFor(() => expect(document.body.textContent).toContain(`shop · Removed ${new Date(removedAt).toLocaleDateString()} · Revision 2`));
  expect(document.body.textContent).not.toMatch(/· (Draft|Adopted) ·/);
  expect(screen.getByRole('button', { name: 'Discard shop' }).hasAttribute('disabled')).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: 'View configuration for shop' }));
  await screen.findByText(/Revision 2 · digest/);
  expect(await screen.findByRole('heading', { name: 'Adopt an existing Compose project' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Map services to containers' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Deployment plan' })).toBeNull();
});
