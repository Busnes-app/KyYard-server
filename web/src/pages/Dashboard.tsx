import { useState } from 'react';
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
    <h1>{mode === 'containers' ? 'Containers' : 'Endpoints'}</h1>
    <p>{mode === 'containers' ? 'Find containers across your Docker hosts. Open a host to inspect resources, read logs, or run an action.' : 'Connect Docker hosts, review enrollment, and inspect their resources.'}</p>
    <StateNotice state={organizations.state} onRetry={organizations.reload} />
    {organizations.state === 'ready' && !org && <EmptyNotice>No organization access yet. Ask an organization administrator to add your account.</EmptyNotice>}
    {org && <>
      <div className="ky-toolbar"><label htmlFor="fleet-org">Organization</label><select id="fleet-org" value={org.id} onChange={(e) => setSelected(e.target.value)} style={{ width: 'auto' }}>{orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select><Link to={orgPath(org.id)}>Manage environments & enroll a host</Link></div>
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
      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous hosts</button>
      <span>{endpoints.data?.length ? `Hosts ${offset + 1}–${offset + endpoints.data.length}` : "No hosts on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next hosts</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No endpoints here yet. <Link to={orgPath(org)}>Create an environment or open an existing one to enroll your first Docker host.</Link></EmptyNotice>}
    {endpoints.state === 'ready' && endpoints.data?.map((e) => <section className="panel" key={e.id}>
      <div className="panel-header"><h2><Link to={endpointPath(org, e.id)}>{e.name}</Link></h2><span className={`badge ${e.state === 'active' ? 'badge-success' : 'badge-secondary'}`}>{e.state}</span></div>
      <p>{e.facts.hostname || e.runtime} · <Link to={endpointPath(org, e.id)}>Open containers & resources</Link></p>
      {mode === 'containers' && <HostContainers org={org} endpoint={e.id} search={search} />}
    </section>)}
  </>;
}
function HostContainers({ org, endpoint, search }: { org: string; endpoint: string; search: string }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}/inventory`);
  if (inventory.state === 'notfound') return <EmptyNotice>Waiting for the agent's first inventory report.</EmptyNotice>;
  const inv = inventory.data;
  const rows = inv?.snapshot.containers.filter((c) => `${c.name} ${c.image}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  return <>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && inv && <>
      <p style={{ margin: '12px 0', fontSize: 12 }}>Observed {new Date(inv.received_at).toLocaleString()}{Date.now() - Date.parse(inv.received_at) > 180000 ? ' · stale inventory' : ''}{inv.snapshot.truncated?.includes('containers') ? ' · container list truncated' : ''} <button className="btn-secondary" onClick={inventory.reload}>Refresh</button></p>
      {rows.length ? <div style={{ overflowX: 'auto' }}><table className="ky-table"><thead><tr><th>Name</th><th>Image</th><th>State</th><th>Ports</th></tr></thead><tbody>{rows.map((c) => <tr key={c.id}><td><Link to={endpointPath(org, endpoint)}>{c.name}</Link></td><td>{c.image}</td><td><span className={`badge ${c.state === 'running' ? 'badge-success' : 'badge-secondary'}`}>{c.state}</span></td><td>{c.ports.map((p) => `${p.host ? `${p.host} → ` : ''}${p.container}/${p.protocol}`).join(', ') || '—'}</td></tr>)}</tbody></table></div> : <EmptyNotice>{search ? 'No matching containers on this host.' : 'No containers on this host.'}</EmptyNotice>}
    </>}
  </>;
}
