import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { RunYAML, parseRunPreview } from './RunYAML';
import { EMPTY_SPEC } from './ContainerConfigurationForm';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('requires an explicit preview and a reviewed confirmation before a Docker run', async () => {
  const fetcher = vi.fn(async () => new Response(JSON.stringify({ containers: [{ ...EMPTY_SPEC, name: 'web', image: { reference: 'nginx:stable', digest: '' } }], workloads: [] })));
  vi.stubGlobal('fetch', fetcher);
  render(<RunYAML base="/api/host" runtime="docker" org="a" endpoint="e" />);
  fireEvent.change(screen.getByRole('textbox', { name: 'YAML' }), { target: { value: 'services:\n  web:\n    image: nginx:stable' } });
  fireEvent.click(screen.getByRole('button', { name: 'Review YAML' }));
  expect(await screen.findByRole('heading', { name: 'web' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Run container' }).hasAttribute('disabled')).toBe(true);
  expect(screen.queryByRole('textbox', { name: 'YAML' })).toBeNull();
  expect(fetcher.mock.calls.length).toBe(1);
});
it('rejects a malformed or cross-runtime preview', () => {
  expect(parseRunPreview({ containers: [], workloads: [] })).toBeNull();
  expect(parseRunPreview({ containers: [{ ...EMPTY_SPEC, env: [{ name: 'TOKEN', value: 123 }] }], workloads: [] })).toBeNull();
});
it('does not echo server or YAML text after refusal', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ diagnostic: { line: 7, reason: 'secret-canary' } }), { status: 422 })));
  render(<RunYAML base="/api/host" runtime="docker" org="a" endpoint="e" />);
  fireEvent.change(screen.getByRole('textbox', { name: 'YAML' }), { target: { value: 'bad yaml' } } );
  fireEvent.click(screen.getByRole('button', { name: 'Review YAML' }));
  expect((await screen.findByRole('alert')).textContent).toContain('near line 7');
  expect(screen.queryByText(/secret-canary/)).toBeNull();
});
