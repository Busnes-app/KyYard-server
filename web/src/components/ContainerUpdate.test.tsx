import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ContainerUpdate } from './ContainerUpdate';
import type { Container } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const container: Container = { id: 'a'.repeat(64), image_id: 'sha256:' + 'b'.repeat(64), name: 'web', image: 'nginx:stable', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: [] };
it('checks explicitly and reports an available update only for this image', async () => {
  const fetcher = vi.fn(async () => new Response(JSON.stringify({ image_id: container.image_id, verdict: 'update_available' })));
  vi.stubGlobal('fetch', fetcher);
  render(<ContainerUpdate base="/api/host" container={container} active org="a" endpoint="e" />);
  expect(fetcher).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Check image update for web' }));
  expect(await screen.findByText('Update available')).toBeTruthy();
  expect(fetcher.mock.calls.length).toBe(1);
  expect(screen.getByRole('link', { name: 'Update image' }).getAttribute('href')).toContain('tab=configuration');
});
it('refuses an update verdict for a different observed image', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ image_id: 'other', verdict: 'up_to_date' }))));
  render(<ContainerUpdate base="/api/host" container={container} active org="a" endpoint="e" />);
  fireEvent.click(screen.getByRole('button', { name: 'Check image update for web' }));
  expect(await screen.findByText('Registry check failed')).toBeTruthy();
  expect(screen.queryByText('Up to date')).toBeNull();
});
