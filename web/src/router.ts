import { useEffect, useState } from 'react';

// Routes are plain paths so deep links, reloads and browser history work without state.
export type Route =
  | { name: 'dashboard' | 'endpoints' | 'backup' | 'suite' | 'notfound' }
  | { name: 'settings'; section: SettingsSection }
  | { name: 'organization' | 'members' | 'access' | 'audit'; org: string }
  | { name: 'environment'; org: string; env: string }
  | { name: 'endpoint' | 'container-new' | 'workload-new'; org: string; endpoint: string }
  | { name: 'workload'; org: string; endpoint: string; namespace: string; kind: string; workload: string }
  | { name: 'container'; org: string; endpoint: string; container: string };

// Settings tabs in order: the personal ones, then the app's. KyYard has no app passwords or
// devices, so there is no Phones & apps tab. Sign-in (provider configuration), Suite and
// Administration are platform administrators'.
export type SettingsSection = 'account' | 'appearance' | 'sign-in' | 'recovery' | 'storage' | 'suite' | 'administration';
const SETTINGS_SECTIONS: { id: SettingsSection; label: string; admin?: true }[] = [
  { id: 'account', label: 'Account' },
  { id: 'appearance', label: 'Appearance' },
  { id: 'sign-in', label: 'Sign-in', admin: true },
  { id: 'recovery', label: 'Recovery' },
  { id: 'storage', label: 'Storage' },
  { id: 'suite', label: 'Suite', admin: true },
  { id: 'administration', label: 'Administration', admin: true },
];
export const settingsSections = (role?: string) => SETTINGS_SECTIONS.filter((s) => !s.admin || role === 'admin');
export const settingsPath = (section: SettingsSection) => `/settings/${section}`;

const NAV_EVENT = 'ky:navigate';
// At least one non-dot character, so a segment can never be `.` or `..`.
const segment = /^(?=.*[A-Za-z0-9_-])[A-Za-z0-9._-]{1,64}$/;
// A Docker container ID as the inventory reports it: lowercase 64 hex, nothing else.
const containerID = /^[0-9a-f]{64}$/;
// Namespace and workload name are Kubernetes DNS labels; kind is one of the configurable three.
export const dnsLabel = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const workloadKinds = ['deployment', 'statefulset', 'daemonset'];

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
  if (parts.length === 1 && (parts[0] === 'endpoints' || parts[0] === 'backup' || parts[0] === 'suite')) return { name: parts[0] };
  if (parts.length === 2 && parts[0] === 'settings' && SETTINGS_SECTIONS.some((s) => s.id === parts[1])) return { name: 'settings', section: parts[1] as SettingsSection };
  if (parts[0] === 'organizations' && parts.length >= 2 && segment.test(parts[1])) {
    const org = parts[1];
    if (parts.length === 2) return { name: 'organization', org };
    if (parts.length === 3 && (parts[2] === 'members' || parts[2] === 'access' || parts[2] === 'audit')) return { name: parts[2], org };
    if (parts.length === 4 && parts[2] === 'environments' && segment.test(parts[3])) return { name: 'environment', org, env: parts[3] };
    if (parts.length === 4 && parts[2] === 'endpoints' && segment.test(parts[3])) return { name: 'endpoint', org, endpoint: parts[3] };
    if (parts.length === 6 && parts[2] === 'endpoints' && segment.test(parts[3]) && parts[4] === 'containers' && parts[5] === 'new') return { name: 'container-new', org, endpoint: parts[3] };
    if (parts.length === 6 && parts[2] === 'endpoints' && segment.test(parts[3]) && parts[4] === 'workloads' && parts[5] === 'new') return { name: 'workload-new', org, endpoint: parts[3] };
    if (parts.length === 8 && parts[2] === 'endpoints' && segment.test(parts[3]) && parts[4] === 'workloads' && dnsLabel.test(parts[5]) && workloadKinds.includes(parts[6]) && dnsLabel.test(parts[7])) return { name: 'workload', org, endpoint: parts[3], namespace: parts[5], kind: parts[6], workload: parts[7] };
    if (parts.length === 6 && parts[2] === 'endpoints' && segment.test(parts[3]) && parts[4] === 'containers' && containerID.test(parts[5])) return { name: 'container', org, endpoint: parts[3], container: parts[5] };
  }
  return { name: 'notfound' };
}

// movedPath is where a route that moved into Settings lives now (null: it did not move). The old
// Appearance page, bare /settings and its ?section= links open a tab; a tab the role cannot see
// opens Account. Other query parameters (the SSO link's ?sso=linked) are kept.
export function movedPath(pathname: string, search: string, role?: string): string | null {
  const query = new URLSearchParams(search);
  let section: string;
  if (pathname === '/appearance') section = 'appearance';
  else if (pathname === '/settings') {
    const old = query.get('section') ?? (query.get('sso') === 'linked' ? 'account' : '');
    query.delete('section');
    // Security and the personal half of Sign-in are Account now.
    section = old === 'security' || (old === 'sign-in' && role !== 'admin') ? 'account' : old;
  } else if (pathname.startsWith('/settings/')) section = pathname.slice('/settings/'.length);
  else return null;
  const allowed = settingsSections(role).some((s) => s.id === section);
  const path = settingsPath(allowed ? (section as SettingsSection) : 'account');
  const rest = query.toString();
  const to = path + (rest ? `?${rest}` : '');
  return to === pathname + search ? null : to;
}

// replace swaps the current history entry, so Back skips a moved route.
export function replace(path: string): void {
  window.history.replaceState(null, '', path);
  window.dispatchEvent(new Event(NAV_EVENT));
}

export function navigate(path: string): void {
  if (path !== window.location.pathname + window.location.search) window.history.pushState(null, '', path);
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

export function useSearchParam(name: string): string {
  const read = () => new URLSearchParams(window.location.search).get(name) ?? '';
  const [value, setValue] = useState(read);
  useEffect(() => {
    const update = () => setValue(read());
    window.addEventListener('popstate', update);
    window.addEventListener(NAV_EVENT, update);
    return () => { window.removeEventListener('popstate', update); window.removeEventListener(NAV_EVENT, update); };
  }, [name]);
  return value;
}

export const orgPath = (org: string, suffix = '') => `/organizations/${encodeURIComponent(org)}${suffix}`;
export const envPath = (org: string, env: string) => orgPath(org, `/environments/${encodeURIComponent(env)}`);
export const endpointPath = (org: string, endpoint: string) => orgPath(org, `/endpoints/${encodeURIComponent(endpoint)}`);
export const containerPath = (org: string, endpoint: string, container: string, tab?: string) => endpointPath(org, endpoint) + `/containers/${encodeURIComponent(container)}` + (tab ? `?tab=${encodeURIComponent(tab)}` : '');
export const runPath = (org: string, endpoint: string) => endpointPath(org, endpoint) + '/containers/new';
export const workloadRunPath = (org: string, endpoint: string) => endpointPath(org, endpoint) + '/workloads/new';
export const workloadPath = (org: string, endpoint: string, namespace: string, kind: string, name: string, tab?: string) => endpointPath(org, endpoint) + `/workloads/${encodeURIComponent(namespace)}/${encodeURIComponent(kind)}/${encodeURIComponent(name)}` + (tab ? `?tab=${encodeURIComponent(tab)}` : '');
