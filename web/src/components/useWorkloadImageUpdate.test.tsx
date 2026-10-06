import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, renderHook } from '@testing-library/react';
import { useWorkloadImageUpdate } from './useWorkloadImageUpdate';
import type { Workload } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const workload: Workload = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: [], paused: false };
const d = 'sha256:' + 'a'.repeat(64);
const container = (name: string, image: string) => ({ name, image, image_id: '', command: [], args: [], env: [], resources: { cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' } });
const read = (unsupported: string[] = []) => ({ target: { namespace: 'shop', kind: 'deployment', name: 'web' }, observed_at: new Date().toISOString(), resource_version: '42', replicas: 1, paused: false, strategy: 'RollingUpdate',
  containers: [container('web', 'ghcr.io/acme/web:2'), container('pinned', `ghcr.io/acme/side@${d}`)], init_containers: [container('init', 'busybox:1')], env_from: [], managed: false, unsupported });
it('applies the read configuration with every tag-tracking container pulled', async () => {
  const calls: [string, RequestInit | undefined][] = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    return url.endsWith('/configuration') ? new Response(JSON.stringify(read())) : new Response(JSON.stringify({ id: 'cmd-1', action: 'workload.apply', outcome: '' }), { status: 202 });
  }));
  const onSent = vi.fn();
  const { result } = renderHook(() => useWorkloadImageUpdate('/api/c', onSent));
  await act(() => result.current.update(workload));
  const body = JSON.parse(String(calls[1]?.[1]?.body));
  expect(calls[1]?.[0]).toBe('/api/c/workloads/shop/deployment/web/apply');
  expect(body.pull).toEqual(['web', 'init']);
  expect(body.resource_version).toBe('42');
  expect(body.confirm).toBe('web');
  expect(body.spec.containers[0].image).toBe('ghcr.io/acme/web:2');
  expect(onSent).toHaveBeenCalledWith(expect.objectContaining({ id: 'cmd-1' }), workload);
});
it('refuses unsupported settings and locks after a lost response', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(read(['env_field_ref'])))));
  const { result } = renderHook(() => useWorkloadImageUpdate('/api/c', () => {}));
  await act(() => result.current.update(workload));
  expect(result.current.error).toContain('Cannot update without losing settings');
  vi.stubGlobal('fetch', vi.fn(async (url: string) => { if (url.endsWith('/configuration')) return new Response(JSON.stringify(read())); throw new TypeError('lost'); }));
  await act(() => result.current.update(workload));
  expect(result.current.lost).toBe(true);
  const fetcher = vi.fn();
  vi.stubGlobal('fetch', fetcher);
  await act(() => result.current.update(workload));
  expect(fetcher).not.toHaveBeenCalled();
});
