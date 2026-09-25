import React, { useState } from 'react';
import { Users } from 'lucide-react';
import { Link } from '../components/Link';
import { Registries } from '../components/Registries';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { orgPath } from '../router';
import { tenantRoles, tenantWrite, useTenantResource, type Member } from '../tenant';

export const Members: React.FC<{ org: string }> = ({ org }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/members`;
  const members = useTenantResource<Member[]>(base);
  const [userID, setUserID] = useState('');
  const [role, setRole] = useState<string>('read_only');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const put = async (id: string, nextRole: string, status: string) => {
    setBusy(true);
    const err = await tenantWrite(`${base}/${encodeURIComponent(id)}`, 'PUT', { role: nextRole, status });
    setBusy(false);
    setMessage(err);
    if (!err) { setUserID(''); members.reload(); }
  };
  const remove = async (m: Member) => {
    if (!window.confirm(`Remove ${m.username} and revoke their access?`)) return;
    setBusy(true);
    const err = await tenantWrite(`${base}/${encodeURIComponent(m.user_id)}`, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) members.reload();
  };

  return (
    <div className="ky-page">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <Users size={24} style={{ color: 'var(--accent)' }} /><span>Members</span>
      </h1>
      <nav aria-label="Administration" className="ky-subnav"><Link to={orgPath(org)}>Environments</Link></nav>
      <section className="panel">
        <StateNotice state={members.state} onRetry={members.reload} />
        {members.state === 'ready' && members.data && (members.data.length === 0 ? <EmptyNotice>No members.</EmptyNotice> : (
          <div style={{ overflowX: 'auto' }}>
            <table className="ky-table ky-responsive-table">
              <thead><tr><th>User</th><th>Role</th><th>Status</th><th><span className="sr-only">Actions</span></th></tr></thead>
              <tbody>
                {members.data.map((m) => (
                  <tr key={m.user_id}>
                    <td data-label="User">{m.username} <span className="font-mono" style={{ fontSize: 11, color: 'var(--ink)' }}>{m.user_id}</span></td>
                    <td data-label="Role">
                      <select aria-label={`Role for ${m.username}`} value={m.role} disabled={busy} onChange={(e) => void put(m.user_id, e.target.value, m.status)}>
                        {tenantRoles.map((r) => <option key={r} value={r}>{r === 'organization_admin' ? 'administrator' : r.replaceAll('_', ' ')}</option>)}
                      </select>
                    </td>
                    <td data-label="Status">
                      <button className="btn-secondary" disabled={busy} onClick={() => void put(m.user_id, m.role, m.status === 'active' ? 'disabled' : 'active')}>
                        {m.status === 'active' ? 'Disable' : 'Enable'}
                      </button>
                    </td>
                    <td data-label="Actions"><button className="btn-danger" disabled={busy} onClick={() => void remove(m)}>Remove</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
        {members.state === 'ready' && (
          <form onSubmit={(e) => { e.preventDefault(); void put(userID.trim(), role, 'active'); }} className="ky-inline-form">
            <label htmlFor="member-id">Add member by user ID</label>
            <input id="member-id" value={userID} onChange={(e) => setUserID(e.target.value)} required maxLength={64} placeholder="usr_…" />
            <select aria-label="Role for new member" value={role} onChange={(e) => setRole(e.target.value)}>
              {tenantRoles.map((r) => <option key={r} value={r}>{r === 'organization_admin' ? 'administrator' : r.replaceAll('_', ' ')}</option>)}
            </select>
            <button type="submit" disabled={busy || !userID.trim()}>Add</button>
          </form>
        )}
        {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
      </section>
      <Registries org={org} />
    </div>
  );
};
