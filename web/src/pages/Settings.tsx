import React from 'react';
import { Settings as SettingsIcon, Database, Palette } from 'lucide-react';
import { ThemeSwitcher } from '../components/ThemeSwitcher';

interface SettingsProps {
  settings: any;
}

export const Settings: React.FC<SettingsProps> = ({ settings }) => {
  return (
    <div style={{ maxWidth: '1080px', margin: '0 auto', padding: '32px 20px' }}>
      <div style={{ marginBottom: '24px' }}>
        <h1 style={{ fontSize: '24px', fontWeight: 'bold', display: 'flex', alignItems: 'center', gap: '10px' }}>
          <SettingsIcon size={24} style={{ color: 'var(--accent)' }} />
          <span>System Settings & Architecture</span>
        </h1>
        <p style={{ color: 'var(--ink)', fontSize: '14px', marginTop: '4px' }}>
          Pluggable database drivers, color theme defaults, and instance configuration.
        </p>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(480px, 1fr))', gap: '20px' }}>
        {/* Pluggable Database Card */}
        <div className="panel">
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '12px' }}>
            <Database size={20} style={{ color: 'var(--accent)' }} />
            <h3 style={{ fontSize: '16px' }}>Pluggable Database Layer</h3>
          </div>
          <p style={{ color: 'var(--ink)', fontSize: '13px', marginBottom: '16px' }}>
            Supports zero-CGO SQLite (default) and PostgreSQL clustering on demand.
          </p>

          <div style={{ background: 'var(--bg)', border: '1px solid var(--line)', borderRadius: '6px', padding: '16px' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '8px', fontSize: '13px' }}>
              <span style={{ color: 'var(--ink)' }}>Active Storage Driver:</span>
              <span className="badge badge-accent font-mono">{settings?.db_driver || 'sqlite'}</span>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '13px' }}>
              <span style={{ color: 'var(--ink)' }}>Swapping Capability:</span>
              <span style={{ color: 'var(--success)' }}>Supported (PostgreSQL / SQLite)</span>
            </div>
          </div>
        </div>

        {/* KySecurity Theme Card */}
        <div className="panel">
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '12px' }}>
            <Palette size={20} style={{ color: 'var(--accent)' }} />
            <h3 style={{ fontSize: '16px' }}>KySecurity Design System</h3>
          </div>
          <p style={{ color: 'var(--ink)', fontSize: '13px', marginBottom: '16px' }}>
            Color tokens: Patina Ky (`#0d0f14`/`#4deeea`), Cyber, Nord, Paper, OLED with Space Grotesk and IBM Plex Mono fonts.
          </p>

          <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            <span style={{ fontSize: '13px', color: 'var(--ink)' }}>Active Color Palette:</span>
            <ThemeSwitcher />
          </div>
        </div>
      </div>
    </div>
  );
};
