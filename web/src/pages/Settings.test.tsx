import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { Settings } from './Settings';
import { AppHeader } from '../components/AppHeader';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const tabs = () => within(screen.getByRole('navigation', { name: 'Settings sections' })).getAllByRole('link');
it('orders the tabs Account, Appearance, then the app sections, each its own path', () => {
 vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 200 })));
 render(<Settings section="storage" settings={{ db_driver: 'sqlite' }} role="admin" />);
 expect(screen.getAllByRole('heading', { level: 1 }).map((h) => h.textContent)).toEqual(['Settings']);
 expect(tabs().map((t) => [t.textContent, t.getAttribute('href')])).toEqual([
  ['Account', '/settings/account'], ['Appearance', '/settings/appearance'], ['Sign-in', '/settings/sign-in'], ['Recovery', '/settings/recovery'],
  ['Storage', '/settings/storage'], ['Suite', '/settings/suite'], ['Administration', '/settings/administration'],
 ]);
 expect(tabs().filter((t) => t.getAttribute('aria-current') === 'page').map((t) => t.textContent)).toEqual(['Storage']);
 expect(screen.getByText('Database: SQLITE')).toBeTruthy();
});
it('keeps provider configuration, Suite and Administration for platform administrators', () => {
 vi.stubGlobal('fetch', vi.fn(async () => new Response('[]', { headers: { 'Content-Type': 'application/json' } })));
 render(<Settings section="suite" settings={null} role="user" />);
 expect(tabs().map((t) => t.textContent)).toEqual(['Account', 'Appearance', 'Recovery', 'Storage']);
 // A tab the role cannot see opens Account.
 expect(tabs().find((t) => t.getAttribute('aria-current') === 'page')?.textContent).toBe('Account');
 expect(screen.queryByRole('link', { name: 'Open suite' })).toBeNull();
 cleanup();
 render(<Settings section="suite" settings={null} role="admin" />);
 expect(screen.getByRole('link', { name: 'Open suite' }).getAttribute('href')).toBe('/suite');
});
it('Account holds security and connecting single sign-on; providers load only on Sign-in', async () => {
 const fetcher = vi.fn(async (_url: string) => new Response('[]', { headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetcher);
 render(<Settings section="account" settings={null} role="admin" />);
 expect(await screen.findByRole('heading', { name: 'Connect single sign-on to your account' })).toBeTruthy();
 expect(fetcher.mock.calls.map(([url]) => String(url))).not.toContain('/api/settings/sso');
 cleanup();
 render(<Settings section="sign-in" settings={null} role="admin" />);
 await screen.findByText('Add sign-in provider');
 expect(fetcher.mock.calls.map(([url]) => String(url))).toContain('/api/settings/sso');
});
it('Appearance is a tab without a page head of its own', () => {
 render(<Settings section="appearance" settings={null} role="user" />);
 expect(screen.queryByRole('heading', { name: 'Appearance' })).toBeNull();
 expect(screen.getByRole('group', { name: 'Theme' })).toBeTruthy();
 expect(screen.getByText(/Match system switches with your device’s setting; Light or Dark keeps one/)).toBeTruthy();
});
it('the header keeps Settings as a row, drops Appearance and signs out with an icon button', () => {
 render(<AppHeader appName="KyYard" route={{ name: 'settings', section: 'appearance' }} user={{ username: 'admin', role: 'admin' }} onLogout={() => {}} />);
 const nav = screen.getByRole('navigation', { name: 'Primary' });
 expect(within(nav).queryByRole('link', { name: 'Appearance' })).toBeNull();
 expect(within(nav).getByRole('link', { name: 'Settings' }).getAttribute('aria-current')).toBe('page');
 const signOut = screen.getAllByRole('button', { name: 'Sign out' });
 expect(signOut).toHaveLength(2);
 for (const b of signOut) {
  expect(b.className).toBe('ky-icon-button');
  expect(b.getAttribute('title')).toBe('Sign out');
  expect(b.textContent).toBe('');
 }
});
