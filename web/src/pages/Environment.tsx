import React, { useState } from 'react';
import { Server } from 'lucide-react';
import { AuditList } from '../components/AuditList';
import { Endpoints } from '../components/Endpoints';
import { Link } from '../components/Link';
import { StateNotice } from '../components/StateNotice';
import { navigate, orgPath } from '../router';
import { tenantWrite, useTenantResource, type Environment as Env } from '../tenant';

export const Environment: React.FC<{ org: string; env: string }> = ({ org, env }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}`;
  const details = useTenantResource<Env>(base);
  const [name, setName] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);

  const rename = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    const err = await tenantWrite(base, 'PATCH', { name });
    setBusy(false);
    setMessage(err);
    if (!err) { setName(''); details.reload(); }
  };
  const remove = async () => {
    if (!window.confirm(`Delete environment "${details.data?.name ?? env}"? Every endpoint in it must already be revoked.`)) return;
    setBusy(true);
    const err = await tenantWrite(base, 'DELETE');
    setBusy(false);
    setMessage(err);
    if (!err) navigate(orgPath(org));
  };

  return (
    <div className="ky-page">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <Server size={24} style={{ color: 'var(--accent)' }} /><span>{details.data?.name ?? env}</span>
      </h1>
      <nav aria-label="Organization sections" className="ky-subnav"><Link to={orgPath(org)}>Back to organization</Link></nav>
      <StateNotice state={details.state} onRetry={details.reload} />
      {details.state === 'ready' && (
        <>
          <Endpoints org={org} env={env} />
          <details className="panel"><summary>Environment settings</summary>
            <form onSubmit={rename} className="ky-inline-form">
              <label htmlFor="env-rename">Rename</label>
              <input id="env-rename" value={name} onChange={(e) => setName(e.target.value)} required maxLength={255} placeholder={details.data?.name} />
              <button type="submit" disabled={busy || !name.trim()}>Save</button>
              <button type="button" className="btn-danger" disabled={busy} onClick={() => void remove()}>Delete environment</button>
            </form>
            {message && <p role="alert" className="dr-alert dr-alert-error">{message}</p>}
          </details>
          <AuditList url={`${base}/audit`} />
        </>
      )}
    </div>
  );
};
