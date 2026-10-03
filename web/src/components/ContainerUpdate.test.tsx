import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ContainerUpdate, useContainerUpdateChecker } from './ContainerUpdate';
import type { Container } from '../tenant';
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const container: Container = { id: 'a'.repeat(64), image_id: 'sha256:' + 'b'.repeat(64), name: 'web', image: 'nginx:stable', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: [] };
function Rows({ containers = [container], active = true, base = '/api/host' }: { containers?: Container[]; active?: boolean; base?: string }) {
  const checkUpdate = useContainerUpdateChecker(base);
  return <>{containers.map((c) => <ContainerUpdate key={`${c.id}/${c.image_id}`} container={c} checkUpdate={checkUpdate} active={active} org="a" endpoint="e" />)}</>;
}
function reply(verdict = 'update_available') { return new Response(JSON.stringify({ image_id: container.image_id, verdict })); }
it('checks visible rows automatically, keeps refreshed results, and supports manual rechecks', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async (_input: RequestInfo | URL) => reply());
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Rows />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Update available')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
  view.rerender(<Rows containers={[{ ...container, status: 'Up 2m' }]} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(fetcher).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole('button', { name: 'Check image update for web' }));
  await act(() => vi.advanceTimersByTimeAsync(5100));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(screen.getByRole('link', { name: 'Update image' }).getAttribute('href')).toContain('tab=configuration');
});
it('paces requests, cancels hidden rows, and caches results across pagination', async () => {
  vi.useFakeTimers();
  const second = { ...container, id: 'c'.repeat(64), name: 'second' };
  const third = { ...container, id: 'd'.repeat(64), name: 'third' };
  const fetcher = vi.fn(async (_input: RequestInfo | URL) => reply());
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Rows containers={[container, second]} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(fetcher).toHaveBeenCalledTimes(1);
  view.rerender(<Rows containers={[third]} />);
  await act(() => vi.advanceTimersByTimeAsync(5099));
  expect(fetcher).toHaveBeenCalledTimes(1);
  await act(() => vi.advanceTimersByTimeAsync(1));
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls[1]?.[0]).toContain(third.id);
  view.rerender(<Rows />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Update available')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(2);
});
it('invalidates a verdict when the image changes and rejects late answers for the previous image', async () => {
  vi.useFakeTimers();
  let finish: (response: Response) => void = () => {};
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; })).mockImplementation(async () => reply());
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Rows />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  view.rerender(<Rows containers={[{ ...container, image_id: 'other' }]} />);
  await act(async () => { finish(reply()); });
  expect(screen.queryByText('Update available')).toBeNull();
  await act(() => vi.advanceTimersByTimeAsync(5100));
  expect(screen.getByText('Update check failed')).toBeTruthy();
});
it.each([
  [403, { code: 'tenant_access_denied' }, 'Update check not permitted'],
  [429, {}, 'Update check limit reached'],
  [409, { code: 'registry_not_configured', error: 'secret diagnostic' }, 'Registry access not configured'],
  [200, { image_id: container.image_id, verdict: 'registry_error', detail: 'unauthorized' }, 'Registry authentication required'],
  [200, { image_id: container.image_id, verdict: 'registry_error', detail: 'not_found' }, 'Image not found in registry'],
  [200, { image_id: container.image_id, verdict: 'registry_error', detail: 'rate_limited' }, 'Registry rate limit reached'],
  [200, { image_id: container.image_id, verdict: 'registry_error', detail: 'unavailable' }, 'Registry unavailable'],
])('shows a fixed reason for status %s', async (status, body, text) => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status })));
  render(<Rows />);
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByText('secret diagnostic')).toBeNull();
  if (status === 409) expect(screen.getByRole('link', { name: 'Configure registry access' }).getAttribute('href')).toBe('/organizations/a/members#registries-heading');
});
it('does not contact an offline endpoint', () => {
  const fetcher = vi.fn();
  vi.stubGlobal('fetch', fetcher);
  render(<Rows active={false} />);
  expect(screen.getByText('Endpoint offline')).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});
