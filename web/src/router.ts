import { useEffect, useState } from 'react';

// Routes are plain paths so deep links, reloads and browser history work without state.
export type Route =
  | { name: 'dashboard' | 'scim' | 'backup' | 'settings' | 'notfound' }
  | { name: 'organization' | 'members' | 'audit'; org: string }
  | { name: 'environment'; org: string; env: string };

const NAV_EVENT = 'ky:navigate';
const segment = /^[A-Za-z0-9._-]{1,64}$/;

export function matchRoute(pathname: string): Route {
  const parts = pathname.split('/').filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return { name: 'dashboard' };
  if (parts.length === 1 && (parts[0] === 'scim' || parts[0] === 'backup' || parts[0] === 'settings')) return { name: parts[0] };
  if (parts[0] === 'organizations' && parts.length >= 2 && segment.test(parts[1])) {
    const org = parts[1];
    if (parts.length === 2) return { name: 'organization', org };
    if (parts.length === 3 && (parts[2] === 'members' || parts[2] === 'audit')) return { name: parts[2], org };
    if (parts.length === 4 && parts[2] === 'environments' && segment.test(parts[3])) return { name: 'environment', org, env: parts[3] };
  }
  return { name: 'notfound' };
}

export function navigate(path: string): void {
  if (path !== window.location.pathname) window.history.pushState(null, '', path);
  window.dispatchEvent(new Event(NAV_EVENT));
}

export function useRoute(): Route {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const update = () => setPath(window.location.pathname);
    window.addEventListener('popstate', update);
    window.addEventListener(NAV_EVENT, update);
    return () => { window.removeEventListener('popstate', update); window.removeEventListener(NAV_EVENT, update); };
  }, []);
  return matchRoute(path);
}

export const orgPath = (org: string, suffix = '') => `/organizations/${encodeURIComponent(org)}${suffix}`;
export const envPath = (org: string, env: string) => orgPath(org, `/environments/${encodeURIComponent(env)}`);
