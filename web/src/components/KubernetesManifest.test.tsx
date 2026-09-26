import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ManifestRegeneration } from './KubernetesManifest';
import type { Endpoint } from '../tenant';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const endpoint = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['shop'] } as Endpoint;

it('posts the typed namespaces and shows the manifest to apply', async () => {
  const saved = vi.fn();
  const fetcher = vi.fn(async () => json({ manifest: 'kind: Role', manifest_file: 'kyyard-agent-prod.yaml', command: 'kubectl apply -f kyyard-agent-prod.yaml', namespaces: ['billing', 'shop'], note: 'Create the namespaces first.' }));
  vi.stubGlobal('fetch', fetcher);
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={saved} />);
  fireEvent.click(screen.getByRole('button', { name: 'Regenerate manifest' }));
  const input = screen.getByLabelText('Namespaces to deploy to');
  expect(input).toHaveProperty('value', 'shop');
  fireEvent.change(input, { target: { value: 'shop, billing  ' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save and show manifest' }));
  const region = await screen.findByRole('region', { name: 'Namespace manifest' });
  expect(region.textContent).toContain('kubectl apply -f kyyard-agent-prod.yaml');
  expect(region.textContent).toContain('Create the namespaces first.');
  const [url, init] = fetcher.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe('/api/organizations/a/endpoints/ep_k/manifest');
  expect(JSON.parse(String(init.body))).toEqual({ namespaces: ['shop', 'billing'] });
  expect(saved).toHaveBeenCalledOnce();
});

it('names a refused list in fixed text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'secret-canary' }, 400)));
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Regenerate manifest' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save and show manifest' }));
  expect((await screen.findByRole('alert')).textContent).toContain('List at most 32 namespaces');
  expect(document.body.textContent).not.toContain('secret-canary');
});

it('copies the manifest beside Download', async () => {
  const writeText = vi.fn(async () => undefined);
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
  vi.stubGlobal('fetch', vi.fn(async () => json({ manifest: 'kind: Role', manifest_file: 'kyyard-agent-prod.yaml', command: 'kubectl apply -f kyyard-agent-prod.yaml', namespaces: ['shop'], note: 'n' })));
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Regenerate manifest' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save and show manifest' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Copy manifest' }));
  expect(writeText).toHaveBeenCalledWith('kind: Role');
  expect((await screen.findByRole('status')).textContent).toBe('Copied.');
});

it('says whether the namespace form is open', () => {
  render(<ManifestRegeneration org="a" endpoint={endpoint} onSaved={vi.fn()} />);
  const toggle = screen.getByRole('button', { name: 'Regenerate manifest' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
});
