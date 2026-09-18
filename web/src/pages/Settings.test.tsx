import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Settings } from './Settings';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('keeps administration separate from the theme picker and loads providers only on demand', async () => {
 const fetcher = vi.fn(async () => new Response('[]', { headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetcher);
 render(<Settings settings={{ db_driver: 'sqlite' }} />);
 expect(screen.getByRole('button', { name: 'Ocean' })).toBeTruthy();
 expect(fetcher).not.toHaveBeenCalled();
 fireEvent.click(screen.getByRole('button', { name: 'Recovery' }));
 expect(screen.getByRole('link', { name: 'Open backup & recovery' })).toBeTruthy();
 expect(screen.queryByRole('button', { name: 'Ocean' })).toBeNull();
 fireEvent.click(screen.getByRole('button', { name: 'Sign-in' }));
 await screen.findByText('Add sign-in provider');
 expect(fetcher).toHaveBeenCalledTimes(1);
});
