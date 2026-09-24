import { LogOut, Server, Settings as SettingsIcon, LayoutDashboard } from 'lucide-react';
import { OrganizationSelect } from './OrganizationSelect';
import { Link } from './Link';
import type { Route } from '../router';

interface AppHeaderProps {
  appName: string;
  route: Route;
  user: { display_name?: string; username: string };
  onLogout: () => void;
}
const navItems = [
  { name: 'dashboard', to: '/', label: 'Containers', icon: LayoutDashboard },
  { name: 'endpoints', to: '/endpoints', label: 'Endpoints', icon: Server },
  { name: 'settings', to: '/settings', label: 'Settings', icon: SettingsIcon },
];
export function AppHeader({ appName, route, user, onLogout }: AppHeaderProps) {
  const org = 'org' in route ? route.org : undefined;
  return <header className="ky-header">
    <Link to="/" className="ky-brand"><img src="/app-icon.png" width={28} height={28} alt="" /><span>{appName || 'KyYard'}</span></Link>
    <nav aria-label="Primary" className="ky-nav">
      {navItems.map(({ name, to, label, icon: Icon }) => {
        const active = route.name === name || (name === 'endpoints' && ['endpoint', 'environment', 'organization'].includes(route.name)) || (name === 'settings' && ['backup', 'members', 'audit'].includes(route.name));
        return <Link key={name} to={to} current={active} className={active ? 'ky-nav-link active' : 'ky-nav-link'}><Icon size={18} /><span>{label}</span></Link>;
      })}
    </nav>
    <div className="ky-account">
      {org && <OrganizationSelect current={org} />}
      <span className="ky-account-name">{user.display_name || user.username}</span>
      <button className="btn-secondary ky-signout" onClick={onLogout} title="Sign out" aria-label="Sign out"><LogOut size={17} /><span>Sign out</span></button>
    </div>
  </header>;
}
