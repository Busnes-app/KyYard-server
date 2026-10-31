import { useEffect, useState } from 'react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ago, useNow } from '../components/containerFacts';
import { endpointPath, orgPath, runPath, workloadRunPath } from '../router';
import { canConfigure, canEnroll, useTenantResource, type MemberOrganization, type Endpoint, type Inventory } from '../tenant';

export function Dashboard({ mode = 'home', platformAdmin = false }: { mode?: 'home' | 'endpoints'; platformAdmin?: boolean }) {
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const [selected, setSelected] = useState('');
  const orgs = organizations.data ?? [];
  const org = orgs.find((o) => o.id === selected) ?? orgs[0];
  return <div className="ky-page ky-fleet-page">
    <header className="ky-page-head">
      <div>
        <h1 className="ky-page-title">{mode === 'home' ? 'Home' : 'Endpoints'}</h1>
        <p className="ky-page-lede">{mode === 'home' ? 'Your Docker hosts and Kubernetes clusters. Choose where to run or manage an application.' : 'Connect Docker hosts and Kubernetes clusters, review enrollment, and inspect their resources.'}</p>
        {org && <dl className="ky-fleet-scope">
          <div><dt>Organization</dt><dd>{orgs.length > 1 ? <select id="fleet-org" aria-label="Access scope" value={org.id} onChange={(e) => setSelected(e.target.value)}>{orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select> : org.name}</dd></div>
          <div><dt>Your role</dt><dd>{org.role?.replaceAll('_', ' ')}</dd></div>
        </dl>}
      </div>
      {org && <Link className="btn btn-secondary" to={orgPath(org.id)}>View environments</Link>}
    </header>
    <StateNotice state={organizations.state} onRetry={organizations.reload} />
    {organizations.state === 'ready' && !org && (platformAdmin
      ? <EmptyNotice>No organization yet. Hosts and clusters belong to an organization: <Link to="/settings?section=administration">create one in Settings → Administration</Link> and name yourself its first administrator.</EmptyNotice>
      : <EmptyNotice>No host access yet. Ask an administrator to add your account.</EmptyNotice>)}
    {org && <Fleet key={`${org.id}-${mode}`} org={org.id} mode={mode} configure={canConfigure(org.role)} enroll={canEnroll(org.role)} />}
  </div>;
}

const kind = (e: Endpoint) => `${e.runtime === 'kubernetes' ? 'Kubernetes cluster' : 'Docker host'}${e.facts.hostname ? ` · ${e.facts.hostname}` : ''}`;
const stateClass = (e: Endpoint) => `badge ${e.state === 'active' ? 'badge-success' : 'badge-secondary'}`;

function Fleet({ org, mode, configure, enroll }: { org: string; mode: 'home' | 'endpoints'; configure: boolean; enroll: boolean }) {
  const [offset, setOffset] = useState(0);
  const [search, setSearch] = useState('');
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/endpoints?offset=${offset}&limit=20`);
  useEffect(() => {
    if (endpoints.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) endpoints.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [endpoints.state === 'denied', endpoints.reload]);
  const hosts = [...(endpoints.data ?? [])].sort((a, b) => Number(b.state === 'active') - Number(a.state === 'active') || a.name.localeCompare(b.name));
  const matches = hosts.filter((e) => `${e.name} ${e.runtime} ${e.facts.hostname ?? ''}`.toLowerCase().includes(search.toLowerCase()));
  const home = mode === 'home';
  return <>
    <div className="ky-toolbar ky-fleet-toolbar">
      {home && <input type="search" aria-label="Find endpoints on this page" placeholder="Search hosts or clusters…" value={search} onChange={(e) => setSearch(e.target.value)} />}
      {home && endpoints.state === 'ready' && <p className="ky-fleet-totals"><strong>{hosts.length} {hosts.length === 1 ? 'endpoint' : 'endpoints'}</strong> · {hosts.filter((e) => e.state === 'active').length} connected · {hosts.filter((e) => e.runtime === 'docker').length} Docker · {hosts.filter((e) => e.runtime === 'kubernetes').length} Kubernetes</p>}
      <button className="btn-secondary" onClick={endpoints.reload}>Refresh endpoints</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No endpoints connected here yet. {enroll ? <>Add a Docker host or Kubernetes cluster. <Link to={orgPath(org)}>Add an endpoint.</Link></> : 'Ask an organization or environment administrator to connect a host or cluster.'}</EmptyNotice>}
    {endpoints.state === 'ready' && hosts.length > 0 && matches.length === 0 && <EmptyNotice>No endpoints match this search on this page. <button className="btn-secondary" onClick={() => setSearch('')}>Clear search</button></EmptyNotice>}
    {endpoints.state === 'ready' && matches.length > 0 && <div className="panel ky-table-flush">
      <table className="ky-table ky-responsive-table ky-fleet-table">
        <thead><tr><th>Endpoint</th><th>Status</th>{home && <><th>Running</th><th>Capacity</th><th>Reported</th></>}<th><span className="sr-only">Actions</span></th></tr></thead>
        <tbody>{matches.map((e) => home && e.state === 'active'
          ? <EndpointRow key={e.id} org={org} endpoint={e} configure={configure} />
          : <tr key={e.id}><td data-label="Endpoint"><EndpointName org={org} e={e} /></td><td data-label="Status"><span className={stateClass(e)}>{e.state}</span></td>{home && <><td data-label="Running">—</td><td data-label="Capacity">—</td><td data-label="Reported">Disconnected</td></>}<td /></tr>)}
        </tbody>
      </table>
    </div>}
    {(offset > 0 || (endpoints.data?.length ?? 0) >= 20) && <div className="ky-pagination">
      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous endpoints</button>
      <span>{endpoints.data?.length ? `Endpoints ${offset + 1}–${offset + endpoints.data.length}` : "No endpoints on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next endpoints</button>
    </div>}
  </>;
}

function EndpointName({ org, e }: { org: string; e: Endpoint }) {
  return <div className="ky-resource-name"><strong><Link to={endpointPath(org, e.id)}>{e.name}</Link></strong><span>{kind(e)}</span></div>;
}

// An active endpoint's row with its inventory summary; others are never read or polled.
function EndpointRow({ org, endpoint: e, configure }: { org: string; endpoint: Endpoint; configure: boolean }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(e.id)}/inventory`, e.state);
  const now = useNow();
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [inventory.state === 'denied', inventory.reload]);
  const report = inventory.data;
  const live = !!(report && Number.isFinite(Date.parse(report.received_at)) && now - Date.parse(report.received_at) <= 180_000 && !inventory.refreshFailed);
  const snapshot = live ? report?.snapshot : undefined;
  const cluster = e.runtime === 'kubernetes';
  const kube = snapshot?.kubernetes;
  const containers = cluster ? [] : snapshot?.containers ?? [];
  const unhealthy = containers.filter((c) => c.health === 'unhealthy').length;
  const exited = containers.filter((c) => c.state === 'exited' || c.state === 'dead').length;
  const notReady = kube?.nodes.filter((n) => !n.ready).length ?? 0;
  const running = !snapshot ? '—' : cluster
    ? kube ? `${kube.workloads.filter((w) => w.ready >= w.desired && w.desired > 0).length} / ${kube.workloads.length}` : '—'
    : `${containers.filter((c) => c.state === 'running').length} / ${containers.length}`;
  const reported = inventory.state !== 'ready' && inventory.state !== 'loading' ? <button className="btn-secondary" onClick={inventory.reload}>Retry</button>
    : !report ? '—'
    : !live ? <span className="badge badge-warning" title="Inventory is stale or its refresh failed. Open the endpoint to check it.">Stale</span>
    : <span title={new Date(report.received_at).toLocaleString()}>{ago(report.received_at, now)}{snapshot?.truncated?.length ? ' · incomplete' : ''}</span>;
  return <tr>
    <td data-label="Endpoint"><EndpointName org={org} e={e} /></td>
    <td data-label="Status"><span className={stateClass(e)}>{e.state}</span></td>
    <td data-label="Running" className="ky-num"><span className="ky-fleet-metric"><strong>{running}</strong> <span className="ky-muted">{cluster ? 'workloads ready' : 'running'}</span>
      {unhealthy > 0 && <span className="badge badge-danger">{unhealthy} unhealthy</span>}
      {exited > 0 && <span className="badge badge-secondary">{exited} exited</span>}
      {notReady > 0 && <span className="badge badge-danger">{notReady} {notReady === 1 ? 'node' : 'nodes'} not ready</span>}</span></td>
    <td data-label="Capacity" className="ky-num">{!snapshot ? '—' : cluster
      ? kube ? `${kube.nodes.length} ${kube.nodes.length === 1 ? 'node' : 'nodes'} · ${kube.pods.length} pods` : '—'
      : <>{snapshot.engine?.cpus || '—'} CPUs · <span>{snapshot.engine?.memory_bytes ? `${(snapshot.engine.memory_bytes / 2 ** 30).toFixed(1)} GiB` : '—'}</span></>}</td>
    <td data-label="Reported">{reported}</td>
    <td className="ky-row-actions">{configure && <Link className="btn btn-secondary" to={cluster ? workloadRunPath(org, e.id) : runPath(org, e.id)}>Run a container{cluster ? ' on Kubernetes' : ''}</Link>}</td>
  </tr>;
}
