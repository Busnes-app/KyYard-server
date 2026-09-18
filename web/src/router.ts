import { useEffect, useState } from 'react';

// Routes are plain paths so deep links, reloads and browser history work without state.
export type Route =
  | { name: 'dashboard' | 'endpoints' | 'backup' | 'settings' | 'notfound' }
  | { name: 'organization' | 'members' | 'audit'; org: string }
  | { name: 'environment'; org: string; env: string }
  | { name: 'endpoint'; org: string; endpoint: string };

const NAV_EVENT = 'ky:navigate';
// At least one non-dot character, so a segment can never be `.` or `..`.
const segment = /^(?=.*[A-Za-z0-9_-])[A-Za-z0-9._-]{1,64}$/;

// decodeURIComponent throws on malformed escapes; a bad link must land on not-found, not a blank page.
function decodeSegments(pathname: string): string[] | null {
  try {
    return pathname.split('/').filter(Boolean).map(decodeURIComponent);
  } catch {
    return null;
  }
}

export function matchRoute(pathname: string): Route {
  const parts = decodeSegments(pathname);
  if (parts === null) return { name: 'notfound' };
  if (parts.length === 0) return { name: 'dashboard' };
  if (parts.length === 1 && (parts[0] === 'endpoints' || parts[0] === 'backup' || parts[0] === 'settings')) return { name: parts[0] };
  if (parts[0] === 'organizations' && parts.length >= 2 && segment.test(parts[1])) {
    const org = parts[1];
    if (parts.length === 2) return { name: 'organization', org };
    if (parts.length === 3 && (parts[2] === 'members' || parts[2] === 'audit')) return { name: parts[2], org };
    if (parts.length === 4 && parts[2] === 'environments' && segment.test(parts[3])) return { name: 'environment', org, env: parts[3] };
    if (parts.length === 4 && parts[2] === 'endpoints' && segment.test(parts[3])) return { name: 'endpoint', org, endpoint: parts[3] };
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
export const endpointPath = (org: string, endpoint: string) => orgPath(org, `/endpoints/${encodeURIComponent(endpoint)}`);
