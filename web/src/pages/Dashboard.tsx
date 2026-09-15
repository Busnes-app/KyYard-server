import React from 'react';
import { Key, Archive, Database, Users, CheckCircle2, ArrowRight } from 'lucide-react';

interface DashboardProps {
  settings: any;
  user: any;
  onNavigate: (tab: string) => void;
}

export const Dashboard: React.FC<DashboardProps> = ({ settings, user, onNavigate }) => {
  const cards = [
    {
      title: 'Feature 0: KyBackup & Recovery',
      desc: 'Encrypted capsule container generation and automated sandboxed restore drills.',
      status: 'Verified Ready',
      statusType: 'success',
      icon: Archive,
      action: () => onNavigate('backup'),
      actionLabel: 'Run Restore Drill',
    },
    {
      title: 'SCIM 2.0 Inbound Provisioning',
      desc: 'RFC 7643/7644 automatic user and group provisioning from enterprise IdPs.',
      status: settings?.scim_enabled ? 'Active' : 'Disabled',
      statusType: settings?.scim_enabled ? 'success' : 'neutral',
      icon: Users,
      action: () => onNavigate('scim'),
      actionLabel: 'Manage Directory',
    },
    {
      title: 'Single Sign-On & Federation',
      desc: 'KySignOn OIDC + Signed Directory Webhooks, Generic OIDC, and SAML 2.0 SP.',
      status: settings?.sso_enabled ? 'Enabled' : 'Disabled',
      statusType: settings?.sso_enabled ? 'success' : 'neutral',
      icon: Key,
      action: () => onNavigate('settings'),
      actionLabel: 'SSO Settings',
    },
    {
      title: 'Pluggable Database Engine',
      desc: `Current storage backend: ${settings?.db_driver?.toUpperCase() || 'SQLITE'} (zero-CGO with WAL mode & foreign keys).`,
      status: `${settings?.db_driver || 'sqlite'} (active)`,
      statusType: 'accent',
      icon: Database,
      action: () => onNavigate('settings'),
      actionLabel: 'Database Config',
    },
  ];

  return (
    <div style={{ maxWidth: '1080px', margin: '0 auto', padding: '32px 20px' }}>
      <div style={{ marginBottom: '32px' }}>
        <h1 style={{ fontSize: '26px', fontWeight: 'bold', marginBottom: '6px' }}>
          Welcome, {user?.display_name || user?.username}!
        </h1>
        <p style={{ color: 'var(--ink)', fontSize: '15px' }}>
          {settings?.app_name || 'Busnes.app'} is initialized on the Ky Server Base platform.
        </p>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: '20px' }}>
        {cards.map((c, i) => {
          const Icon = c.icon;
          return (
            <div key={i} className="panel" style={{ display: 'flex', flexDirection: 'column', justifyContent: 'space-between' }}>
              <div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '12px' }}>
                  <div style={{ padding: '8px', background: 'var(--accent-soft)', borderRadius: '6px', color: 'var(--accent)' }}>
                    <Icon size={20} />
                  </div>
                  <span className={`badge badge-${c.statusType === 'success' ? 'success' : c.statusType === 'accent' ? 'accent' : 'secondary'}`}>
                    {c.statusType === 'success' && <CheckCircle2 size={12} />}
                    {c.status}
                  </span>
                </div>
                <h3 style={{ fontSize: '16px', marginBottom: '6px' }}>{c.title}</h3>
                <p style={{ color: 'var(--ink)', fontSize: '13px', lineHeight: 1.5 }}>{c.desc}</p>
              </div>

              <div style={{ marginTop: '20px', borderTop: '1px solid var(--line)', paddingTop: '12px' }}>
                <button
                  type="button"
                  className="btn-secondary"
                  style={{ width: '100%', justifyContent: 'space-between', fontSize: '13px' }}
                  onClick={c.action}
                >
                  <span>{c.actionLabel}</span>
                  <ArrowRight size={14} />
                </button>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
};
