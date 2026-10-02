import { useEffect, useState } from 'react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { useNow } from '../components/containerFacts';
import { endpointPath, orgPath, runPath, workloadRunPath } from '../router';
import { canConfigure, useTenantResource, type MemberOrganization, type Endpoint, type Inventory } from '../tenant';

export function Dashboard({ mode = 'home' }: { mode?: 'home' | 'endpoints' }) {
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const [selected, setSelected] = useState('');
  const orgs = organizations.data ?? [];
  const org = orgs.find((o) => o.id === selected) ?? orgs[0];
  return <div className="ky-page ky-fleet-page">
    <div className="ky-page-heading"><h1>{mode === 'home' ? 'Home' : 'Endpoints'}</h1>{org && <Link className="btn btn-secondary" to={orgPath(org.id)}>Environments & add endpoint</Link>}</div>
    <p>{mode === 'home' ? 'Your Docker hosts and Kubernetes clusters. Choose where to run or manage an application.' : 'Connect Docker hosts and Kubernetes clusters, review enrollment, and inspect their resources.'}</p>
    <StateNotice state={organizations.state} onRetry={organizations.reload} />
    {organizations.state === 'ready' && !org && <EmptyNotice>No host access yet. Ask an administrator to add your account.</EmptyNotice>}
    {org && <>
      {orgs.length > 1 && <div className="ky-toolbar"><label htmlFor="fleet-org">Access scope</label><select id="fleet-org" value={org.id} onChange={(e) => setSelected(e.target.value)} style={{ width: 'auto' }}>{orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select></div>}
      <Fleet key={`${org.id}-${mode}`} org={org.id} mode={mode} configure={canConfigure(org.role)} />
    </>}
  </div>;
}
function Fleet({ org, mode, configure }: { org: string; mode: 'home' | 'endpoints'; configure: boolean }) {
  const [offset, setOffset] = useState(0);
  const [search, setSearch] = useState('');
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/endpoints?offset=${offset}&limit=20`);
  useEffect(() => {
    if (endpoints.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) endpoints.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [endpoints.state === 'denied', endpoints.reload]);
  const hosts = [...(endpoints.data ?? [])].sort((a, b) => Number(b.state === 'active') - Number(a.state === 'active') || a.name.localeCompare(b.name));
  return <>
    <div className="ky-toolbar">
      {mode === 'home' && <input type="search" aria-label="Find endpoints on this page" placeholder="Search hosts or clusters…" value={search} onChange={(e) => setSearch(e.target.value)} />}

      <button className="btn-secondary" onClick={endpoints.reload}>Refresh endpoints</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No endpoints connected here yet. Add a Docker host or Kubernetes cluster. <Link to={orgPath(org)}>Add an endpoint.</Link></EmptyNotice>}
    {mode === 'home' && endpoints.state === 'ready' && <p className="ky-fleet-totals">This page: <strong>{hosts.length} endpoints</strong> · {hosts.filter((e) => e.state === 'active').length} connected · {hosts.filter((e) => e.runtime === 'docker').length} Docker · {hosts.filter((e) => e.runtime === 'kubernetes').length} Kubernetes</p>}
    {endpoints.state === 'ready' && (hosts.filter((e) => `${e.name} ${e.runtime} ${e.facts.hostname ?? ''}`.toLowerCase().includes(search.toLowerCase()))).map((e) => <section className="panel" key={e.id}>
      <div className="panel-header"><h2><Link to={endpointPath(org, e.id)}>{e.name}</Link></h2><span className={`badge ${e.state === 'active' ? 'badge-success' : 'badge-secondary'}`}>{e.state}</span></div>
      <p>{e.runtime === 'kubernetes' ? 'Kubernetes cluster' : 'Docker host'}{e.facts.hostname ? ` · ${e.facts.hostname}` : ''}</p>
      {mode === 'home' && (e.state === 'active' ? <EndpointSummary org={org} endpoint={e} configure={configure} /> : <p>Disconnected. Open the endpoint to inspect its historical inventory.</p>)}

    </section>)}
    {(offset > 0 || (endpoints.data?.length ?? 0) >= 20) && <div className="ky-pagination">      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous endpoints</button>
      <span>{endpoints.data?.length ? `Endpoints ${offset + 1}–${offset + endpoints.data.length}` : "No endpoints on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next endpoints</button>
</div>}
  </>;
}
function EndpointSummary({ org, endpoint: e, configure }: { org: string; endpoint: Endpoint; configure: boolean }) {
  const active = e.state === 'active';
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(e.id)}/inventory`, e.state);
  const now = useNow();
  useEffect(() => {
    if (inventory.state === 'denied' || !active) return;
    const timer = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [active, inventory.state === 'denied', inventory.reload]);
  const report = inventory.data;
  const live = active && report && Number.isFinite(Date.parse(report.received_at)) && now - Date.parse(report.received_at) <= 180_000 && !inventory.refreshFailed;
  const snapshot = report?.snapshot;
  const cluster = e.runtime === 'kubernetes';
  const kube = snapshot?.kubernetes;
  return <>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    <div className="ky-endpoint-summary">
      <div><span>{cluster ? 'Workloads ready' : 'Containers running'}</span><strong>{live ? cluster ? kube ? `${kube.workloads.filter((w) => w.ready >= w.desired && w.desired > 0).length} / ${kube.workloads.length}` : '—' : `${snapshot?.containers.filter((c) => c.state === 'running').length ?? 0} / ${snapshot?.containers.length ?? 0}` : '—'}</strong></div>
      <div><span>{cluster ? 'Nodes ready' : 'CPUs'}</span><strong>{live ? cluster ? kube ? `${kube.nodes.filter((n) => n.ready).length} / ${kube.nodes.length}` : '—' : snapshot?.engine?.cpus || '—' : '—'}</strong></div>
      <div><span>{cluster ? 'Pods' : 'Memory'}</span><strong>{live ? cluster ? kube?.pods.length ?? '—' : snapshot?.engine?.memory_bytes ? `${(snapshot.engine.memory_bytes / 2 ** 30).toFixed(1)} GiB` : '—' : '—'}</strong></div>
    </div>
    {inventory.state === 'ready' && report && <p className="text-muted">{!live ? 'Inventory is stale or its refresh failed. Open the endpoint to check it.' : `Reported ${new Date(report.received_at).toLocaleString()}${snapshot?.truncated?.length ? ' · inventory is incomplete' : ''}`}</p>}
    <div className="ky-toolbar"><Link className="btn btn-secondary" to={endpointPath(org, e.id)}>{cluster ? 'Open cluster' : 'Open containers'}</Link>{configure && active && <Link className="btn" to={cluster ? workloadRunPath(org, e.id) : runPath(org, e.id)}>Run a container{cluster ? ' on Kubernetes' : ''}</Link>}</div>
  </>;
}
