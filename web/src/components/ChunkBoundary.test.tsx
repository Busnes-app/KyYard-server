import { afterEach, expect, it, vi } from 'vitest';
import { lazy, Suspense } from 'react';
import { cleanup, render, screen } from '@testing-library/react';
import { ChunkBoundary } from './ChunkBoundary';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('shows a notice with Reload, not a blank app, when a lazy chunk fails to load', async () => {
  vi.spyOn(console, 'error').mockImplementation(() => {});
  const Broken = lazy(() => Promise.reject(new Error('Failed to fetch dynamically imported module')));
  render(<main><h1>Container</h1><ChunkBoundary what="terminal"><Suspense fallback={<p>Loading terminal…</p>}><Broken /></Suspense></ChunkBoundary></main>);
  expect((await screen.findByRole('alert')).textContent).toContain('The terminal could not load.');
  expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Container' })).toBeTruthy();
});
