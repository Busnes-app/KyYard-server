import React from 'react';
import { KeyRound } from 'lucide-react';
import { Link } from '../components/Link';
import { Registries } from '../components/Registries';
import { ServiceTokens } from '../components/ServiceTokens';
import { AgentTokens } from '../components/AgentTokens';
import { orgPath } from '../router';
import { canManageAgentTokens, canManageServiceTokens, useTenantResource, type MemberOrganization } from '../tenant';

// Access holds what the organization lets software do, beside Members for people: the service
// tokens other Ky apps read with (Pair kyPulse), agent tokens, and the registries update checks
// and pulls may use. Each panel keeps its own role gate; the server enforces them regardless.
export const Access: React.FC<{ org: string }> = ({ org }) => {
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const callerRole = (Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role;
  return (
    <div className="ky-page">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <KeyRound size={24} style={{ color: 'var(--accent)' }} /><span>Access</span>
      </h1>
      <nav aria-label="Administration" className="ky-subnav"><Link to={orgPath(org)}>Environments</Link><Link to={orgPath(org, '/members')}>Members</Link></nav>
      <p>Integrations and registries for this organization: tokens other apps use to read it, and the registries update checks may query.</p>
      {canManageServiceTokens(callerRole) && <ServiceTokens org={org} />}
      {canManageAgentTokens(callerRole) && <AgentTokens org={org} />}
      <Registries org={org} />
    </div>
  );
};
