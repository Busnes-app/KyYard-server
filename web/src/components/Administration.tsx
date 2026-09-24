import React, { useState } from 'react';
import { secureFetch } from '../api';
import { EmptyNotice, StateNotice } from './StateNotice';
import { offlineWrite, refusal, tenantWrite, useTenantResource, type AdminOrganization, type AdminUser, type CreatedUser } from '../tenant';

const forbidden = 'Administrator role required.';
const orgTexts = {
  forbidden,
  invalid: 'Organization name must be 1 to 64 printable characters, and a first administrator is required.',
  notFound: 'That user no longer exists. Refresh and choose another.',
  conflict: { organization_exists: 'An organization with that name already exists.', user_inactive: 'That user is not active. Choose an active user.' },
};
const userTexts = {
  forbidden,
  invalid: 'Username must be 3 to 64 of a-z, 0-9, dot, underscore or hyphen; display name 1 to 128 printable characters.',
  conflict: { username_exists: 'That username is already taken.' },
};
const date = (iso: string) => new Date(iso).toLocaleDateString();

// Platform administration: organizations and local accounts. Settings shows it to platform admins only.
export const Administration: React.FC = () => {
  const orgs = useTenantResource<AdminOrganization[]>('/api/admin/organizations');
  const users = useTenantResource<AdminUser[]>('/api/admin/users');
  const [orgForm, setOrgForm] = useState({ name: '', admin: '' });
  const [orgMessage, setOrgMessage] = useState({ ok: false, text: '' });
  const [userForm, setUserForm] = useState({ username: '', displayName: '', role: 'user' });
  const [userMessage, setUserMessage] = useState('');
  const [created, setCreated] = useState<CreatedUser | null>(null);
  const [copied, setCopied] = useState('');
  const [busy, setBusy] = useState(false);
  const active = users.data?.filter((u) => u.status === 'active') ?? [];

  const createOrg = async () => {
    setBusy(true);
    const err = await tenantWrite('/api/admin/organizations', 'POST', { name: orgForm.name.trim(), admin_user_id: orgForm.admin }, orgTexts);
    setBusy(false);
    setOrgMessage(err ? { ok: false, text: err } : { ok: true, text: 'Organization created. Its admin adds members on the organization page.' });
    if (!err) { setOrgForm({ name: '', admin: '' }); orgs.reload(); }
  };

  const createUser = async () => {
    setBusy(true); setUserMessage(''); setCreated(null); setCopied('');
    try {
      const resp = await secureFetch('/api/admin/users', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: userForm.username.trim(), display_name: userForm.displayName.trim(), role: userForm.role }) });
      if (!resp.ok) { setUserMessage(await refusal(resp, userTexts)); return; }
      setCreated(await resp.json() as CreatedUser);
      setUserForm({ username: '', displayName: '', role: 'user' });
      users.reload();
    } catch {
      setUserMessage(offlineWrite);
    } finally { setBusy(false); }
  };

  const copy = async (text: string) => {
    try { await navigator.clipboard.writeText(text); setCopied('Copied.'); } catch { setCopied('Copy failed; select the field and copy it by hand.'); }
  };

  return <>
    <section className="panel" aria-labelledby="admin-orgs-heading">
      <h2 id="admin-orgs-heading">Organizations</h2>
      <StateNotice state={orgs.state} onRetry={orgs.reload} />
      {orgs.state === 'ready' && orgs.data && (orgs.data.length === 0 ? <EmptyNotice>No organizations yet.</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Name</th><th>Members</th><th>Created</th></tr></thead>
            <tbody>{orgs.data.map((o) => <tr key={o.id}><td data-label="Name">{o.name}</td><td data-label="Members">{o.members}</td><td data-label="Created">{date(o.created_at)}</td></tr>)}</tbody>
          </table>
        </div>
      ))}
      {users.state === 'ready' && (
        <form onSubmit={(e) => { e.preventDefault(); void createOrg(); }} className="ky-inline-form" aria-label="Create an organization">
          <label htmlFor="admin-org-name">Organization name</label>
          <input id="admin-org-name" value={orgForm.name} onChange={(e) => setOrgForm({ ...orgForm, name: e.target.value })} required maxLength={64} />
          <label htmlFor="admin-org-admin">First administrator</label>
          <select id="admin-org-admin" value={orgForm.admin} onChange={(e) => setOrgForm({ ...orgForm, admin: e.target.value })} required>
            <option value="">Choose an active user</option>
            {active.map((u) => <option key={u.id} value={u.id}>{u.username} ({u.display_name})</option>)}
          </select>
          <button type="submit" disabled={busy || !orgForm.name.trim() || !orgForm.admin}>Create organization</button>
        </form>
      )}
      {orgMessage.text && <p role={orgMessage.ok ? 'status' : 'alert'} className={orgMessage.ok ? undefined : 'dr-alert dr-alert-error'}>{orgMessage.text}</p>}
    </section>
    <section className="panel" aria-labelledby="admin-users-heading">
      <h2 id="admin-users-heading">Users</h2>
      <StateNotice state={users.state} onRetry={users.reload} />
      {users.state === 'ready' && users.data && (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Username</th><th>Display name</th><th>Role</th><th>Status</th><th>Provider</th></tr></thead>
            <tbody>{users.data.map((u) => <tr key={u.id}><td data-label="Username" className="font-mono">{u.username}</td><td data-label="Display name">{u.display_name}</td><td data-label="Role">{u.role}</td><td data-label="Status">{u.status}</td><td data-label="Provider">{u.sso_provider}</td></tr>)}</tbody>
          </table>
        </div>
      )}
      <form onSubmit={(e) => { e.preventDefault(); void createUser(); }} className="ky-inline-form" aria-label="Create a local user">
        <label htmlFor="admin-user-name">Username</label>
        <input id="admin-user-name" value={userForm.username} onChange={(e) => setUserForm({ ...userForm, username: e.target.value })} required minLength={3} maxLength={64} pattern="[a-z0-9._\-]+" autoComplete="off" />
        <label htmlFor="admin-user-display">Display name</label>
        <input id="admin-user-display" value={userForm.displayName} onChange={(e) => setUserForm({ ...userForm, displayName: e.target.value })} required maxLength={128} autoComplete="off" />
        <label htmlFor="admin-user-role">Role</label>
        <select id="admin-user-role" value={userForm.role} onChange={(e) => setUserForm({ ...userForm, role: e.target.value })}>
          <option value="user">User</option>
          <option value="admin">Platform administrator</option>
        </select>
        <button type="submit" disabled={busy || !userForm.username.trim() || !userForm.displayName.trim()}>Create user</button>
      </form>
      {userMessage && <p role="alert" className="dr-alert dr-alert-error">{userMessage}</p>}
      {created && <div role="status">
        <label htmlFor="admin-temp-password">Temporary password for {created.username}</label>
        <div className="ky-inline-form">
          <input id="admin-temp-password" className="font-mono" readOnly value={created.temporary_password} onFocus={(e) => e.currentTarget.select()} />
          <button type="button" className="btn-secondary" onClick={() => void copy(created.temporary_password)}>Copy</button>
        </div>
        <p>Give this password to the person out of band. They must change it at first sign-in. It is not shown again.</p>
        {copied && <p>{copied}</p>}
      </div>}
    </section>
  </>;
};
