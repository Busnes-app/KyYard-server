import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Settings } from './Settings';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it('keeps administration separate from the theme picker and loads providers only on demand', async () => {
 const fetcher = vi.fn(async () => new Response('[]', { headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetcher);
 render(<Settings settings={{ db_driver: 'sqlite' }} role="admin" />);
 expect(screen.getByRole('radio', { name: /^Ocean/ })).toBeTruthy();
 expect(fetcher).not.toHaveBeenCalled();
 fireEvent.click(screen.getByRole('button', { name: 'Recovery' }));
 expect(screen.getByRole('link', { name: 'Open backup & recovery' })).toBeTruthy();
 expect(screen.queryByRole('radio', { name: /^Ocean/ })).toBeNull();
 fireEvent.click(screen.getByRole('button', { name: 'Sign-in' }));
 await screen.findByText('Add sign-in provider');
 expect(fetcher).toHaveBeenCalledTimes(2);
});
it('links the suite page for platform administrators only', () => {
 render(<Settings settings={null} role="user" />);
 expect(screen.queryByRole('button', { name: 'Suite' })).toBeNull();
 cleanup();
 render(<Settings settings={null} role="admin" />);
 fireEvent.click(screen.getByRole('button', { name: 'Suite' }));
 expect(screen.getByRole('link', { name: 'Open suite' }).getAttribute('href')).toBe('/suite');
});
