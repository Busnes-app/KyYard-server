import React, { useState } from 'react';
import { Smartphone, LogOut, Shield, Users, Settings as SettingsIcon, LayoutDashboard, Archive } from 'lucide-react';
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
      <header
        style={{
          background: 'var(--panel)',
          borderBottom: '1px solid var(--line)',
          padding: '0 20px',
          height: '60px',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          position: 'sticky',
          top: 0,
          zIndex: 100,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: '24px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', fontWeight: 'bold', fontSize: '18px', color: 'var(--accent)' }}>
            <Shield size={22} />
            <span>{appName || 'Busnes.app'}</span>
          </div>

          <nav style={{ display: 'flex', gap: '4px' }}>
            {navItems.map((item) => {
              const Icon = item.icon;
              const active = activeTab === item.id;
              return (
                <button
                  key={item.id}
                  onClick={() => onTabChange(item.id)}
                  style={{
                    background: active ? 'var(--accent-soft)' : 'transparent',
                    color: active ? 'var(--accent)' : 'var(--ink)',
                    border: active ? '1px solid var(--accent)' : '1px solid transparent',
                    padding: '6px 12px',
                    fontSize: '13px',
                    borderRadius: '6px',
                  }}
                >
                  <Icon size={16} />
                  <span>{item.label}</span>
                </button>
              );
            })}
          </nav>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
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
              <button
                className="btn-secondary"
                style={{ padding: '6px', color: 'var(--danger)' }}
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
