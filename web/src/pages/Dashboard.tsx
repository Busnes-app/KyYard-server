import { ContainerPorts } from '../components/ContainerPorts';
import { useEffect, useState } from 'react';
import { ResourceTable } from '../components/ResourceTable';
import { PAGE_KINDS } from '../components/PodControls';
import { usePagination } from '../components/Pagination';
import { displayName } from '../components/Endpoints';
import { ContainerControls } from '../components/ContainerControls';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { IPCell, StateCell } from '../components/ContainerCells';
import { uptime, useNow } from '../components/containerFacts';
import { containerPath, dnsLabel, endpointPath, orgPath, workloadPath } from '../router';
import { canExec, useTenantResource, type MemberOrganization, type Endpoint, type Inventory } from '../tenant';

export function Dashboard({ mode = 'containers' }: { mode?: 'containers' | 'endpoints' }) {
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const [selected, setSelected] = useState('');
  const orgs = organizations.data ?? [];
  const org = orgs.find((o) => o.id === selected) ?? orgs[0];
  return <div className="ky-page ky-fleet-page">
    <div className="ky-page-heading"><h1>{mode === 'containers' ? 'Containers & workloads' : 'Endpoints'}</h1>{org && <Link className="btn btn-secondary" to={orgPath(org.id)}>Environments & add endpoint</Link>}</div>
    <p>{mode === 'containers' ? 'Browse Docker containers and Kubernetes workloads by endpoint.' : 'Connect Docker hosts and Kubernetes clusters, review enrollment, and inspect their resources.'}</p>
    <StateNotice state={organizations.state} onRetry={organizations.reload} />
    {organizations.state === 'ready' && !org && <EmptyNotice>No host access yet. Ask an administrator to add your account.</EmptyNotice>}
    {org && <>
      {orgs.length > 1 && <div className="ky-toolbar"><label htmlFor="fleet-org">Access scope</label><select id="fleet-org" value={org.id} onChange={(e) => setSelected(e.target.value)} style={{ width: 'auto' }}>{orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select></div>}
      <Fleet key={`${org.id}-${mode}`} org={org.id} mode={mode} exec={canExec(org.role)} />
    </>}
  </div>;
}
function Fleet({ org, mode, exec }: { org: string; mode: 'containers' | 'endpoints'; exec: boolean }) {
  const [offset, setOffset] = useState(0);
  const [selectedHost, setSelectedHost] = useState('');
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/endpoints?offset=${offset}&limit=20`);
  useEffect(() => {
    if (endpoints.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) endpoints.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [endpoints.state === 'denied', endpoints.reload]);
  const hosts = endpoints.data ?? [];
  const host = hosts.find((e) => e.id === selectedHost) ?? hosts.find((e) => e.state === 'active') ?? hosts[0];
  return <>
    <div className="ky-toolbar">
      {mode === 'containers' && hosts.length > 1 && <><label htmlFor="fleet-host">Endpoint</label><select id="fleet-host" value={host?.id ?? ''} onChange={(event) => setSelectedHost(event.target.value)}>{hosts.map((e) => <option key={e.id} value={e.id}>{e.name} · {e.runtime === 'kubernetes' ? 'Kubernetes' : 'Docker'} · {e.state}</option>)}</select></>}
      <button className="btn-secondary" onClick={endpoints.reload}>Refresh endpoints</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No endpoints connected here yet. Add a Docker host or Kubernetes cluster. <Link to={orgPath(org)}>Add an endpoint.</Link></EmptyNotice>}
    {endpoints.state === 'ready' && (mode === 'containers' ? (host ? [host] : []) : hosts).map((e) => <section className="panel" key={e.id}>
      <div className="panel-header"><h2><Link to={endpointPath(org, e.id)}>{e.name}</Link></h2><span className={`badge ${e.state === 'active' ? 'badge-success' : 'badge-secondary'}`}>{e.state}</span></div>
      <p>{e.facts.hostname || e.runtime} · <Link to={endpointPath(org, e.id)}>{e.runtime === 'kubernetes' ? 'Open cluster & resources' : 'Open containers & resources'}</Link></p>
      {mode === 'containers' && (e.state === 'active' ? <EndpointInventory org={org} host={e} exec={exec} /> : <>
        <EmptyNotice>This endpoint is {e.state}. Its saved inventory is historical and does not describe the server running KyYard now.</EmptyNotice>
        <details><summary>Show last reported inventory</summary><EndpointInventory org={org} host={e} exec={false} /></details>
      </>)}
    </section>)}
    {(offset > 0 || (endpoints.data?.length ?? 0) >= 20) && <div className="ky-pagination">      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous endpoints</button>
      <span>{endpoints.data?.length ? `Endpoints ${offset + 1}–${offset + endpoints.data.length}` : "No endpoints on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next endpoints</button>
</div>}
  </>;
}
function EndpointInventory({ org, host, exec }: { org: string; host: Endpoint; exec: boolean }) {
  const { id: endpoint, name: hostName } = host;
  const active = host.state === 'active';
  const cluster = host.runtime === 'kubernetes';
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}/inventory`);
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [inventory.state === 'denied', inventory.reload]);
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState('');
  const now = useNow();
  const inv = inventory.data;
  const rows = inv?.snapshot.containers.filter((c) => `${c.name} ${c.image}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  const pagination = usePagination(rows, search);
  const stale = !!inv && (!Number.isFinite(Date.parse(inv.received_at)) || now - Date.parse(inv.received_at) > 180000);
  const workloads = inv?.snapshot.kubernetes?.workloads.filter((w) => `${w.namespace}/${w.name} ${w.images.join(' ')}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  if (inventory.state === 'notfound') return <EmptyNotice>Waiting for the agent's first inventory report.</EmptyNotice>;
  return <>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && inv && <>
      <p className="ky-inventory-status">Observed {new Date(inv.received_at).toLocaleString()}{stale ? ' · stale inventory' : ''}{!active ? ' · last reported state, not live' : ''}{inventory.refreshFailed ? ' · refresh failed; showing the previous report' : ''}{inv.snapshot.truncated?.length ? ` · lists truncated: ${inv.snapshot.truncated.join(', ')}` : ''} <button className="btn-secondary" onClick={inventory.reload}>Refresh</button></p>
      {cluster ? (inv.snapshot.kubernetes ? <>
        <p>{inv.snapshot.kubernetes.nodes.filter((n) => n.ready).length} of {inv.snapshot.kubernetes.nodes.length} nodes ready · {inv.snapshot.kubernetes.pods.length} pods · <Link to={endpointPath(org, endpoint)}>View pods, services & cluster details</Link></p>
        <div className="ky-toolbar"><input aria-label="Find workloads on this cluster" type="search" placeholder="Find by namespace, workload or image…" value={search} onChange={(event) => setSearch(event.target.value)} /></div>
        <ResourceTable key={search} title="Workloads" rows={workloads} rowKey={(w) => `${w.kind}/${w.namespace}/${w.name}`} empty={search ? 'No matching workloads on this cluster.' : 'No workloads reported on this cluster.'} head={['Workload', 'Kind', 'Ready', 'Images']} render={(w) => [
          Object.hasOwn(PAGE_KINDS, w.kind) && dnsLabel.test(w.namespace) && dnsLabel.test(w.name) ? <Link to={workloadPath(org, endpoint, w.namespace, PAGE_KINDS[w.kind] ?? '', w.name)}>{displayName(w.namespace)}/{displayName(w.name)}</Link> : `${displayName(w.namespace)}/${displayName(w.name)}`,
          w.kind, `${w.ready}/${w.desired}${w.paused ? ' (paused)' : ''}`, w.images.map(displayName).join(', '),
        ]} />
      </> : <EmptyNotice>The agent has not reported the cluster yet.</EmptyNotice>) : <>
      <div className="ky-toolbar"><input aria-label="Find containers on this host" type="search" placeholder="Find by container name or image…" value={search} onChange={(event) => setSearch(event.target.value)} /><span>{rows.length} containers</span></div>
      {pagination.controls}
      {status && <p role="status">{status}</p>}
      {rows.length ? <div style={{ overflowX: 'auto' }}><table className="ky-table ky-responsive-table"><thead><tr><th>Container</th><th>Status</th><th>Uptime</th><th>IP</th><th>Ports</th><th>Actions</th></tr></thead><tbody>{pagination.rows.map((c) => <tr key={c.id}><td data-label="Container"><div className="ky-resource-name"><strong><Link to={containerPath(org, endpoint, c.id)}>{c.name}</Link></strong><span>{c.image}</span></div></td><td data-label="Status"><StateCell c={c} /></td><td data-label="Uptime">{active && !stale ? uptime(c.started_at, now) || '—' : '—'}</td><td data-label="IP"><IPCell c={c} /></td><td data-label="Ports"><ContainerPorts ports={c.ports} /></td><td data-label="Actions"><ContainerControls key={c.id} base={`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`} container={c} active={active && !stale} scope={`Host ${displayName(hostName)} · Endpoint ${endpoint}`} onRefresh={inventory.reload} canExec={exec} org={org} endpoint={endpoint} onStatus={setStatus} /></td></tr>)}</tbody></table></div> : <EmptyNotice>{search ? 'No matching containers on this host.' : inv.snapshot.engine && !inv.snapshot.engine.version ? 'Docker is unavailable. Check the host’s Docker service and socket access.' : 'No containers on this host.'}</EmptyNotice>}
      </>}
    </>}
  </>;
}
