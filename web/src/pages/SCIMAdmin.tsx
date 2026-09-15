import React, { useEffect, useState } from 'react';
import { Users, Copy, Check } from 'lucide-react';

export const SCIMAdmin: React.FC = () => {
  const [users, setUsers] = useState<any[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [scimToken, setScimToken] = useState<string>('');
  const [copied, setCopied] = useState<boolean>(false);

  useEffect(() => {
    const fetchUsers = async () => {
      try {
        const setResp = await fetch('/api/settings');
        const setJson = await setResp.json();
        setScimToken(setJson.extra_settings?.scim_token || 'Bearer token configured in environment');

        // Fetch users through SCIM
        const resp = await fetch('/scim/v2/Users');
        if (resp.ok) {
          const data = await resp.json();
          setUsers(data.Resources || []);
        }
      } catch (err) {
        console.error('Failed to load SCIM directory', err);
      } finally {
        setLoading(false);
      }
    };

    fetchUsers();
  }, []);

  const copyToken = () => {
    navigator.clipboard.writeText(scimToken);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div style={{ maxWidth: '1080px', margin: '0 auto', padding: '32px 20px' }}>
      <div style={{ marginBottom: '24px' }}>
        <h1 style={{ fontSize: '24px', fontWeight: 'bold', display: 'flex', alignItems: 'center', gap: '10px' }}>
          <Users size={24} style={{ color: 'var(--accent)' }} />
          <span>SCIM 2.0 Directory & Inbound Provisioning</span>
        </h1>
        <p style={{ color: 'var(--ink)', fontSize: '14px', marginTop: '4px' }}>
          RFC 7643 and RFC 7644 inbound automated provisioning for Okta, Microsoft Entra ID (Azure AD), and KySignOn.
        </p>
      </div>

      <div className="panel" style={{ marginBottom: '24px' }}>
        <h3 style={{ fontSize: '16px', marginBottom: '8px' }}>SCIM 2.0 Connection Details</h3>
        <p style={{ color: 'var(--ink)', fontSize: '13px', marginBottom: '16px' }}>
          Configure these credentials in your Enterprise Identity Provider SCIM application.
        </p>

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
          <div>
            <label style={{ display: 'block', fontSize: '12px', color: 'var(--ink)', marginBottom: '4px' }}>
              SCIM Base URL
            </label>
            <input
              type="text"
              readOnly
              value={`${window.location.origin}/scim/v2`}
              style={{ fontFamily: 'var(--font-mono)', fontSize: '13px' }}
            />
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '12px', color: 'var(--ink)', marginBottom: '4px' }}>
              SCIM Bearer Token
            </label>
            <div style={{ display: 'flex', gap: '8px' }}>
              <input
                type="password"
                readOnly
                value={scimToken}
                style={{ fontFamily: 'var(--font-mono)', fontSize: '13px' }}
              />
              <button type="button" className="btn-secondary" onClick={copyToken}>
                {copied ? <Check size={16} /> : <Copy size={16} />}
              </button>
            </div>
          </div>
        </div>
      </div>

      <div className="panel">
        <h3 style={{ fontSize: '16px', marginBottom: '16px' }}>Provisioned Accounts ({users.length})</h3>

        {loading ? (
          <p style={{ color: 'var(--ink)', fontSize: '13px' }}>Loading directory...</p>
        ) : users.length === 0 ? (
          <p style={{ color: 'var(--ink)', fontSize: '13px' }}>No users provisioned yet via SCIM or local directory.</p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '13px' }}>
              <thead>
                <tr style={{ borderBottom: '1px solid var(--line)', textAlign: 'left', color: 'var(--ink)' }}>
                  <th style={{ padding: '8px 12px' }}>Username</th>
                  <th style={{ padding: '8px 12px' }}>Display Name</th>
                  <th style={{ padding: '8px 12px' }}>Email</th>
                  <th style={{ padding: '8px 12px' }}>Status</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id} style={{ borderBottom: '1px solid var(--line)' }}>
                    <td style={{ padding: '10px 12px', fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{u.userName}</td>
                    <td style={{ padding: '10px 12px' }}>{u.displayName || '—'}</td>
                    <td style={{ padding: '10px 12px', color: 'var(--ink)' }}>{u.emails?.[0]?.value || '—'}</td>
                    <td style={{ padding: '10px 12px' }}>
                      <span className={`badge badge-${u.active ? 'success' : 'danger'}`}>
                        {u.active ? 'Active' : 'Inactive'}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
};
