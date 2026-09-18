import React from 'react';
import { LogOut, Container, Shield, Settings as SettingsIcon, LayoutDashboard } from 'lucide-react';
import { OrganizationSelect } from './OrganizationSelect';
import { Link } from './Link';
import type { Route } from '../router';

interface AppHeaderProps {
  appName: string;
  route: Route;
  user: any;
  onLogout: () => void;
}

const navItems = [
  { name: 'dashboard', to: '/', label: 'Containers', icon: LayoutDashboard },
  { name: 'endpoints', to: '/endpoints', label: 'Endpoints', icon: Shield },
  { name: 'settings', to: '/settings', label: 'Settings', icon: SettingsIcon },
];

export const AppHeader: React.FC<AppHeaderProps> = ({ appName, route, user, onLogout }) => {
  const org = 'org' in route ? route.org : undefined;

  return (
    <>
      <header className="ky-header">
        <div className="ky-header-row">
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', fontWeight: 'bold', fontSize: '18px', color: 'var(--accent)' }}>
            <Container size={22} />
            <span>{appName || 'KyYard'}</span>
          </div>
          <nav aria-label="Primary" className="ky-nav">
            {navItems.map((item) => {
              const Icon = item.icon;
              const active = route.name === item.name;
              return (
                <Link key={item.name} to={item.to} current={active} className={active ? 'ky-nav-link active' : 'ky-nav-link'}>
                  <Icon size={16} />
                  <span>{item.label}</span>
                </Link>
              );
            })}
          </nav>
        </div>

        <div className="ky-header-row">
          {org && <OrganizationSelect current={org} />}
          {user && (
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', borderLeft: '1px solid var(--line)', paddingLeft: '12px' }}>
              <div style={{ fontSize: '13px', textAlign: 'right' }}>
                <div style={{ fontWeight: 600, color: 'var(--ink-strong)' }}>{user.display_name || user.username}</div>
                <div style={{ fontSize: '11px', color: 'var(--ink)' }}>{user.role}</div>
              </div>
              <button className="btn-secondary" style={{ padding: '6px', color: 'var(--danger)' }} onClick={onLogout} title="Sign out" aria-label="Sign out">
                <LogOut size={16} />
              </button>
            </div>
          )}
        </div>
      </header>


    </>
  );
};
