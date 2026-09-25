import React, { useState } from 'react';
import { Smartphone, LogOut, Users, Settings as SettingsIcon, LayoutDashboard, Archive } from 'lucide-react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { QRPairingModal } from './QRPairingModal';

interface AppHeaderProps {
  appName: string;
  activeTab: string;
  onTabChange: (tab: string) => void;
  user: any;
  onLogout: () => void;
}

export const AppHeader: React.FC<AppHeaderProps> = ({ appName, activeTab, onTabChange, user, onLogout }) => {
  const [showPairing, setShowPairing] = useState<boolean>(false);

  const navItems = [
    { id: 'dashboard', label: 'Overview', icon: LayoutDashboard },
    { id: 'scim', label: 'Directory & SCIM', icon: Users },
    { id: 'backup', label: 'KyBackup (Feature 0)', icon: Archive },
    { id: 'settings', label: 'Settings & DB', icon: SettingsIcon },
  ];

  return (
    <>
      <header className="app-header">
        <div className="app-brand">
            <img src="/app-icon.png" width={28} height={28} alt="" />
            <span>{appName || 'KyYard'}</span>
        </div>

          <nav className="app-nav" aria-label="Primary">
            {navItems.map((item) => {
              const Icon = item.icon;
              const active = activeTab === item.id;
              return (
                <button
                  key={item.id}
                  onClick={() => onTabChange(item.id)}
                  className={active ? 'active' : undefined}
                  aria-current={active ? 'page' : undefined}
                >
                  <Icon size={16} />
                  <span>{item.label}</span>
                </button>
              );
            })}
          </nav>

        <div className="app-header-actions">
          <button className="btn-secondary app-pair" onClick={() => setShowPairing(true)}>
            <Smartphone size={16} style={{ color: 'var(--accent)' }} />
            <span>Pair Device</span>
          </button>

          <ThemeSwitcher />

          {user && (
            <div className="app-user">
              <div className="app-user-copy">
                <div style={{ fontWeight: 600, color: 'var(--ink-strong)' }}>{user.display_name || user.username}</div>
                <div style={{ fontSize: '11px', color: 'var(--ink)' }}>{user.role}</div>
              </div>
              <button
                className="btn-secondary app-logout"
                onClick={onLogout}
                title="Sign out"
                aria-label="Sign out"
              >
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
