import React from 'react';
import { Link } from './Link';
import { navigate, orgPath } from '../router';
import { useTenantResource, type MemberOrganization } from '../tenant';

// The list is the caller's own memberships, so the selector never names an organization
// the user cannot open. It re-reads whenever the current organization changes.
export const OrganizationSelect: React.FC<{ current?: string }> = ({ current }) => {
  const { state, data } = useTenantResource<MemberOrganization[]>('/api/organizations', current ?? '');
  if (state === 'loading') return <span style={{ fontSize: 13, color: 'var(--ink)' }}>Loading organizations…</span>;
  if (state !== 'ready' || !data) return <span style={{ fontSize: 13, color: 'var(--ink)' }}>Organizations unavailable</span>;
  if (data.length === 0) return <span style={{ fontSize: 13, color: 'var(--ink)' }}>No organizations</span>;
  if (data.length === 1) return <Link className="ky-workspace" to={orgPath(data[0].id)}>{data[0].name}</Link>;
  const known = current && data.some((o) => o.id === current);
  return (
    <select
      aria-label="Organization"
      value={known ? current : ''}
      onChange={(e) => { if (e.target.value) navigate(orgPath(e.target.value)); }}
      style={{ maxWidth: 220, padding: '6px 8px', fontSize: 13 }}
    >
      <option value="">Choose an organization…</option>
      {data.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
    </select>
  );
};
