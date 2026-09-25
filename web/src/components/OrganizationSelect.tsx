import React from 'react';
import { navigate, orgPath } from '../router';
import { useTenantResource, type MemberOrganization } from '../tenant';

// The list is the caller's own memberships, so the selector never names an organization
// the user cannot open. It re-reads whenever the current organization changes.
export const OrganizationSelect: React.FC<{ current?: string }> = ({ current }) => {
  const { state, data } = useTenantResource<MemberOrganization[]>('/api/organizations', current ?? '');
  if (state === 'loading') return <span style={{ fontSize: 13, color: 'var(--ink)' }}>Loading access…</span>;
  if (state !== 'ready' || !data) return <span style={{ fontSize: 13, color: 'var(--ink)' }}>Access unavailable</span>;
  if (data.length === 0) return <span style={{ fontSize: 13, color: 'var(--ink)' }}>No access</span>;
  if (data.length === 1) return null;
  const known = current && data.some((o) => o.id === current);
  return (
    <select
      aria-label="Access scope"
      value={known ? current : ''}
      onChange={(e) => { if (e.target.value) navigate(orgPath(e.target.value)); }}
      style={{ maxWidth: 220, padding: '6px 8px', fontSize: 13 }}
    >
      <option value="">Choose access scope…</option>
      {data.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
    </select>
  );
};
