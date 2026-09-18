import React, { useState } from 'react';
import { Building2, Users, ScrollText } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { envPath, orgPath } from '../router';
import { tenantWrite, useTenantResource, type Environment, type MemberOrganization } from '../tenant';

export const Organization: React.FC<{ org: string }> = ({ org }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}`;
  const details = useTenantResource<MemberOrganization>(base);
  const envs = useTenantResource<Environment[]>(`${base}/environments`);
  const [name, setName] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    const err = await tenantWrite(`${base}/environments`, 'POST', { name });
    setBusy(false);
    setMessage(err);
    if (!err) { setName(''); envs.reload(); }
  };

  return (
    <div className="ky-page">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <Building2 size={24} style={{ color: 'var(--accent)' }} />
        <span>Environments</span>
      </h1>
      <nav aria-label="Administration" className="ky-subnav">
        <Link to={orgPath(org, '/members')}><Users size={14} /> Members</Link>
        <Link to={orgPath(org, '/audit')}><ScrollText size={14} /> Audit</Link>
      </nav>
      <StateNotice state={details.state} onRetry={() => { details.reload(); envs.reload(); }} />
      {details.state === 'ready' && (
        <section className="panel" aria-labelledby="env-heading">
          <div className="panel-header"><h2 id="env-heading" style={{ fontSize: 16 }}>Environments</h2></div>
          <StateNotice state={envs.state} onRetry={envs.reload} />
          {envs.state === 'ready' && envs.data && (envs.data.length === 0
            ? <EmptyNotice>No environments yet. Create one to start enrolling endpoints.</EmptyNotice>
            : <ul className="ky-list">{envs.data.map((e) => <li key={e.id}><Link to={envPath(org, e.id)}>{e.name}</Link></li>)}</ul>)}
          <form onSubmit={create} className="ky-inline-form">
            <label htmlFor="env-name">New environment</label>
            <input id="env-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={255} placeholder="Production" />
            <button type="submit" disabled={busy || !name.trim()}>Create</button>
          </form>
          {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
        </section>
      )}
    </div>
  );
};
