import { LogOut, Server, Settings as SettingsIcon, LayoutDashboard } from 'lucide-react';
import { OrganizationSelect } from './OrganizationSelect';
import { Link } from './Link';
import { LicenseFooter } from './LicenseFooter';
import type { Route } from '../router';

interface AppHeaderProps {
  appName: string;
  version?: string;
  route: Route;
  user: { display_name?: string; username: string; role?: string };
  onLogout: () => void;
}
const navItems = [
  { name: 'dashboard', to: '/', label: 'Home', icon: LayoutDashboard },
  { name: 'endpoints', to: '/endpoints', label: 'Endpoints', icon: Server },
  { name: 'settings', to: '/settings/account', label: 'Settings', icon: SettingsIcon },
];
// Icon only, named by aria-label; title shows the same word on hover. KyYard is an admin console:
// Settings is a navigation row, so there is no Settings button beside it.
const SignOut = ({ onLogout }: { onLogout: () => void }) =>
  <button type="button" className="ky-icon-button" aria-label="Sign out" title="Sign out" onClick={onLogout}><LogOut size={16} aria-hidden="true" /></button>;

export function AppHeader({ appName, version, route, user, onLogout }: AppHeaderProps) {
  const org = 'org' in route ? route.org : undefined;
  const role = user.role === 'admin' ? 'Administrator' : 'Everyday account';
  // A display name that repeats the role line says nothing; show the username instead.
  const name = user.display_name && user.display_name !== role ? user.display_name : user.username;
  return <header className="ky-sidebar">
    <div className="ky-mobile-head">
      <div className="ky-brand"><img className="ky-mark" src="/app-icon.png" width={28} height={28} alt="" /><span>{appName || 'KyYard'}</span></div>
      <div className="ky-mobile-actions"><SignOut onLogout={onLogout} /></div>
    </div>
    <nav aria-label="Primary" className="ky-nav">
      {navItems.map(({ name, to, label, icon: Icon }) => {
        const active = route.name === name || (name === 'endpoints' && ['endpoint', 'environment', 'organization'].includes(route.name)) || (name === 'settings' && ['backup', 'suite', 'members', 'audit'].includes(route.name));
        return <Link key={name} to={to} current={active} className="ky-nav-item"><Icon size={16} aria-hidden="true" /><span>{label}</span></Link>;
      })}
    </nav>
    <div className="ky-sidebar-foot">
      {org && <div className="ky-sidebar-scope"><OrganizationSelect current={org} /></div>}
      <div className="ky-sidebar-user"><div className="ky-user">
        <div className="ky-user-copy">
          <p className="ky-user-name">{name}</p>
          <p className="ky-user-role">{role}</p>
        </div>
        <div className="ky-user-actions"><SignOut onLogout={onLogout} /></div>
      </div></div>
      <LicenseFooter appName={appName || 'KyYard'} version={version} />
    </div>
  </header>;
}
