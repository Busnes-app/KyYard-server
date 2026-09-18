import { useState } from 'react';
import { ContainerControls } from '../components/ContainerControls';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { endpointPath, orgPath } from '../router';
import { useTenantResource, type MemberOrganization, type Endpoint, type Inventory } from '../tenant';

export function Dashboard({ mode = 'containers' }: { mode?: 'containers' | 'endpoints' }) {
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const [selected, setSelected] = useState('');
  const orgs = organizations.data ?? [];
  const org = orgs.find((o) => o.id === selected) ?? orgs[0];
  return <div className="ky-page">
    <div className="ky-page-heading"><h1>{mode === 'containers' ? 'Containers' : 'Endpoints'}</h1></div>
    <p>{mode === 'containers' ? 'Your containers, across every connected Docker host.' : 'Connect Docker hosts, review enrollment, and inspect their resources.'}</p>
    <StateNotice state={organizations.state} onRetry={organizations.reload} />
    {organizations.state === 'ready' && !org && <EmptyNotice>No organization access yet. Ask an organization administrator to add your account.</EmptyNotice>}
    {org && <>
      <div className="ky-toolbar">{orgs.length > 1 && <><label htmlFor="fleet-org">Organization</label><select id="fleet-org" value={org.id} onChange={(e) => setSelected(e.target.value)} style={{ width: 'auto' }}>{orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select></>}<Link className="btn btn-secondary" to={orgPath(org.id)}>Environments & add host</Link></div>
      <Fleet key={`${org.id}-${mode}`} org={org.id} mode={mode} />
    </>}
  </div>;
}
function Fleet({ org, mode }: { org: string; mode: 'containers' | 'endpoints' }) {
  const [offset, setOffset] = useState(0);
  const [search, setSearch] = useState('');
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/endpoints?offset=${offset}&limit=20`);
  return <>
    <div className="ky-toolbar">
      {mode === 'containers' && <input aria-label="Find containers on this page" type="search" placeholder="Find by container name or image…" value={search} onChange={(e) => setSearch(e.target.value)} />}
      <button className="btn-secondary" onClick={endpoints.reload}>Refresh hosts</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No hosts connected here yet. The standard installation connects local Docker automatically; check Docker access if it is missing. <Link to={orgPath(org)}>Add another Docker host.</Link></EmptyNotice>}
    {endpoints.state === 'ready' && endpoints.data?.map((e) => <section className="panel" key={e.id}>
      <div className="panel-header"><h2><Link to={endpointPath(org, e.id)}>{e.name}</Link></h2><span className={`badge ${e.state === 'active' ? 'badge-success' : 'badge-secondary'}`}>{e.state}</span></div>
      <p>{e.facts.hostname || e.runtime} · <Link to={endpointPath(org, e.id)}>Open containers & resources</Link></p>
      {mode === 'containers' && <HostContainers org={org} endpoint={e.id} search={search} active={e.state === 'active'} />}
    </section>)}
    {(offset > 0 || (endpoints.data?.length ?? 0) >= 20) && <div className="ky-pagination">      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous hosts</button>
      <span>{endpoints.data?.length ? `Hosts ${offset + 1}–${offset + endpoints.data.length}` : "No hosts on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next hosts</button>
</div>}
  </>;
}
function HostContainers({ org, endpoint, search, active }: { org: string; endpoint: string; search: string; active: boolean }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}/inventory`);
  if (inventory.state === 'notfound') return <EmptyNotice>Waiting for the agent's first inventory report.</EmptyNotice>;
  const inv = inventory.data;
  const rows = inv?.snapshot.containers.filter((c) => `${c.name} ${c.image}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  return <>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && inv && <>
      <p style={{ margin: '12px 0', fontSize: 12 }}>Observed {new Date(inv.received_at).toLocaleString()}{Date.now() - Date.parse(inv.received_at) > 180000 ? ' · stale inventory' : ''}{inv.snapshot.truncated?.includes('containers') ? ' · container list truncated' : ''} <button className="btn-secondary" onClick={inventory.reload}>Refresh</button></p>
      {rows.length ? <div style={{ overflowX: 'auto' }}><table className="ky-table ky-responsive-table"><thead><tr><th>Container</th><th>State</th><th>Ports</th><th>Actions</th></tr></thead><tbody>{rows.map((c) => <tr key={c.id}><td data-label="Container"><div className="ky-resource-name"><strong>{c.name}</strong><span>{c.image}</span></div></td><td data-label="State"><span className={`badge ${c.state === 'running' ? 'badge-success' : c.state === 'exited' || c.state === 'dead' ? 'badge-danger' : 'badge-secondary'}`}>{c.state}</span></td><td data-label="Ports">{c.ports.map((p) => `${p.host ? `${p.host} → ` : ''}${p.container}/${p.protocol}`).join(', ') || '—'}</td><td data-label="Actions"><ContainerControls key={c.id} base={`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`} container={c} active={active} scope={`Organization ${org} · Endpoint ${endpoint}`} onRefresh={inventory.reload} /></td></tr>)}</tbody></table></div> : <EmptyNotice>{search ? 'No matching containers on this host.' : inv.snapshot.engine && !inv.snapshot.engine.version ? 'Docker is unavailable. Check the host’s Docker service and socket access.' : 'No containers on this host.'}</EmptyNotice>}
    </>}
  </>;
}
