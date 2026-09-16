import React, { useState } from 'react';
import { Smartphone, LogOut, Shield, Users, Settings as SettingsIcon, LayoutDashboard, Archive } from 'lucide-react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { QRPairingModal } from './QRPairingModal';
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
  { name: 'dashboard', to: '/', label: 'Overview', icon: LayoutDashboard },
  { name: 'scim', to: '/scim', label: 'Directory & SCIM', icon: Users },
  { name: 'backup', to: '/backup', label: 'KyBackup (Feature 0)', icon: Archive },
  { name: 'settings', to: '/settings', label: 'Settings & DB', icon: SettingsIcon },
];

export const AppHeader: React.FC<AppHeaderProps> = ({ appName, route, user, onLogout }) => {
  const [showPairing, setShowPairing] = useState<boolean>(false);
  const org = 'org' in route ? route.org : undefined;

  return (
    <>
      <header className="ky-header">
        <div className="ky-header-row">
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', fontWeight: 'bold', fontSize: '18px', color: 'var(--accent)' }}>
            <Shield size={22} />
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
          <OrganizationSelect current={org} />
          <button className="btn-secondary" style={{ padding: '6px 12px', fontSize: '13px' }} onClick={() => setShowPairing(true)}>
            <Smartphone size={16} style={{ color: 'var(--accent)' }} />
            <span>Pair Device</span>
          </button>
          <ThemeSwitcher />
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

      {showPairing && <QRPairingModal onClose={() => setShowPairing(false)} />}
    </>
  );
};
