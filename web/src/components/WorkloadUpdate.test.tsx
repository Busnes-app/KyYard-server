import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useWorkloadUpdateChecker, WorkloadUpdate } from './WorkloadUpdate';
import type { Pod, Workload } from '../tenant';
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
const workload: Workload = { kind: 'Deployment', namespace: 'shop', name: 'web', desired: 1, ready: 1, updated: 1, images: ['ghcr.io/acme/web:2'], paused: false };
const pod: Pod = { namespace: 'shop', name: 'web-1', phase: 'Running', node: 'n', owner_kind: 'Deployment', owner_name: 'web', started_at: '', containers: [{ name: 'web', image: 'ghcr.io/acme/web:2', image_id: 'ghcr.io/acme/web@sha256:' + 'a'.repeat(64), state: 'running', reason: '', ready: true, restart_count: 0 }] };
function Row({ pods = [pod], onUpdate = () => {} }: { pods?: Pod[]; onUpdate?: () => void }) {
  const check = useWorkloadUpdateChecker('/api/c');
  return <WorkloadUpdate workload={workload} pods={pods} active org="a" checkUpdate={check} onUpdate={onUpdate} />;
}
const reply = (verdict: string, workloadName = 'shop/deployment/web') => new Response(JSON.stringify({ workload: workloadName, verdict, detail: '', containers: [] }));
it('checks the workload route and shows the verdict and the update button', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async (_input: RequestInfo | URL) => reply('update_available'));
  vi.stubGlobal('fetch', fetcher);
  const onUpdate = vi.fn();
  render(<Row onUpdate={onUpdate} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(String(fetcher.mock.calls[0]?.[0])).toBe('/api/c/workloads/shop/deployment/web/updates/check');
  expect(screen.getByText('Update available')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Update image' }));
  expect(onUpdate).toHaveBeenCalledTimes(1);
});
it('shows managed without an update button, and refuses an answer for another workload', async () => {
  vi.useFakeTimers();
  vi.stubGlobal('fetch', vi.fn(async () => reply('managed')));
  const view = render(<Row />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Managed by an application')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Update image' })).toBeNull();
  view.unmount();
  vi.stubGlobal('fetch', vi.fn(async () => reply('up_to_date', 'shop/deployment/api')));
  render(<Row pods={[{ ...pod, name: 'web-2' }]} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText('Update check failed')).toBeTruthy();
});
it('re-checks when the pods change images', async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async () => reply('up_to_date'));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<Row />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  view.rerender(<Row pods={[{ ...pod, containers: [{ ...pod.containers[0]!, image_id: 'ghcr.io/acme/web@sha256:' + 'b'.repeat(64) }] }]} />);
  await act(() => vi.advanceTimersByTimeAsync(5100));
  expect(fetcher).toHaveBeenCalledTimes(2);
});
