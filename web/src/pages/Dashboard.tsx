import { useCallback, useEffect, useState } from 'react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ago, containerStatus, needsAttention, useNow } from '../components/containerFacts';
import { displayName, endpointTone } from '../components/Endpoints';
import { Status, type Tone } from '../components/Status';
import { ACTIONS as WORKLOAD_ACTIONS } from '../components/workloadTexts';
import { containerPath, endpointPath, orgPath, runPath, workloadRunPath } from '../router';
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
      ? <EmptyNotice>No organization yet. Hosts and clusters belong to an organization: <Link to="/settings/administration">create one in Settings → Administration</Link> and name yourself its first administrator.</EmptyNotice>
      : <EmptyNotice>No host access yet. Ask an administrator to add your account.</EmptyNotice>)}
    {org && <Fleet key={`${org.id}-${mode}`} org={org.id} mode={mode} configure={canConfigure(org.role)} enroll={canEnroll(org.role)} />}
  </div>;
}

const kind = (e: Endpoint) => `${e.runtime === 'kubernetes' ? 'Kubernetes cluster' : 'Docker host'}${e.facts.hostname ? ` · ${e.facts.hostname}` : ''}`;

// Triage: problems lead Home, worst first; the endpoint table follows in the same order.
interface Problem { key: string; tone: 'danger' | 'warning'; word: string; name: string; to: string; detail: string }
interface Activity { id: string; at: string; text: string; outcome: string; to: string; where: string }
interface Report { problems: Problem[]; activity: Activity[] }
interface Command { id: string; action: string; outcome: string; container_id?: string; reference?: string; created_at: string }
const rank = (t: Tone) => t === 'danger' ? 0 : t === 'warning' ? 1 : 2;
const worst = (problems: Problem[]) => Math.min(2, ...problems.map((p) => rank(p.tone)));
const ATTENTION_SHOWN = 10;
const ACTIVITY_SHOWN = 8;

// Fixed texts: a command's action and outcome are never shown raw.
const ACTIONS: Record<string, string> = { 'container.start': 'Start', 'container.stop': 'Stop', 'container.restart': 'Restart', 'container.recreate': 'Recreate', 'container.remove': 'Remove', 'image.pull': 'Pull image', 'image.remove': 'Remove image', ...WORKLOAD_ACTIONS };
const OUTCOMES: Record<string, [string, Tone]> = { '': ['Pending', undefined], succeeded: ['Done', 'success'], failed: ['Failed', 'danger'], denied: ['Refused', 'danger'], timed_out: ['No answer', 'warning'], unknown: ['Unknown', 'warning'] };

// An endpoint that is not connected has no inventory; its state alone is the problem.
const endpointProblem = (org: string, e: Endpoint): Problem[] => e.state === 'offline'
  ? [{ key: e.id, tone: 'danger', word: 'Disconnected', name: e.name, to: endpointPath(org, e.id), detail: kind(e) }]
  : e.state === 'pending' ? [{ key: e.id, tone: 'warning', word: 'Awaiting approval', name: e.name, to: endpointPath(org, e.id), detail: kind(e) }] : [];

function Fleet({ org, mode, configure, enroll }: { org: string; mode: 'home' | 'endpoints'; configure: boolean; enroll: boolean }) {
  const [offset, setOffset] = useState(0);
  const [search, setSearch] = useState('');
  const [reports, setReports] = useState<Readonly<Record<string, Report>>>({});
  const onReport = useCallback((id: string, report: Report | null) => setReports((old) => {
    const next = { ...old };
    if (report) next[id] = report; else delete next[id];
    return next;
  }), []);
  const now = useNow();
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/endpoints?offset=${offset}&limit=20`);
  useEffect(() => {
    if (endpoints.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) endpoints.reload(); }, 30_000);
    return () => window.clearInterval(timer);
  }, [endpoints.state === 'denied', endpoints.reload]);
  const home = mode === 'home';
  const problemsOf = (e: Endpoint) => e.state === 'active' ? reports[e.id]?.problems ?? [] : endpointProblem(org, e);
  const hosts = [...(endpoints.data ?? [])].sort((a, b) => (home ? worst(problemsOf(a)) - worst(problemsOf(b)) : 0) || Number(b.state === 'active') - Number(a.state === 'active') || a.name.localeCompare(b.name));
  const match = (e: Endpoint) => `${e.name} ${e.runtime} ${e.facts.hostname ?? ''}`.toLowerCase().includes(search.toLowerCase());
  const matches = hosts.filter(match);
  const attention = home ? hosts.flatMap(problemsOf).sort((a, b) => rank(a.tone) - rank(b.tone)) : [];
  const activity = home ? hosts.flatMap((e) => reports[e.id]?.activity ?? []).sort((a, b) => b.at.localeCompare(a.at)).slice(0, ACTIVITY_SHOWN) : [];
  return <>
    {endpoints.state === 'ready' && attention.length > 0 && <section className="panel ky-triage" aria-labelledby="attention-heading">
      <h2 id="attention-heading" className="ky-table-title">Needs attention <span>({attention.length})</span></h2>
      <ul className="ky-triage-list">{attention.slice(0, ATTENTION_SHOWN).map((p) => <li key={p.key}>
        <Status tone={p.tone}>{p.word}</Status>
        <Link to={p.to}>{displayName(p.name)}</Link>
        <span className="ky-triage-detail" title={p.detail}>{displayName(p.detail)}</span>
      </li>)}</ul>
      {attention.length > ATTENTION_SHOWN && <p className="ky-triage-more">{attention.length - ATTENTION_SHOWN} more. Open an endpoint for its full list.</p>}
    </section>}
    <div className="ky-toolbar ky-fleet-toolbar">
      {home && <input type="search" aria-label="Find endpoints on this page" placeholder="Search hosts or clusters…" value={search} onChange={(e) => setSearch(e.target.value)} />}
      {home && endpoints.state === 'ready' && <p className="ky-fleet-totals"><strong>{hosts.length} {hosts.length === 1 ? 'endpoint' : 'endpoints'}</strong> · {hosts.filter((e) => e.state === 'active').length} connected · {hosts.filter((e) => e.runtime === 'docker').length} Docker · {hosts.filter((e) => e.runtime === 'kubernetes').length} Kubernetes</p>}
      <button className="btn-secondary" onClick={endpoints.reload}>Refresh endpoints</button>
    </div>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && endpoints.data?.length === 0 && <EmptyNotice>No endpoints connected here yet. {enroll ? <>Add a Docker host or Kubernetes cluster. <Link to={orgPath(org)}>Add an endpoint.</Link></> : 'Ask an organization or environment administrator to connect a host or cluster.'}</EmptyNotice>}
    {endpoints.state === 'ready' && hosts.length > 0 && matches.length === 0 && <EmptyNotice>No endpoints match this search on this page. <button className="btn-secondary" onClick={() => setSearch('')}>Clear search</button></EmptyNotice>}
    {/* Rows stay mounted while a search hides them, so their reports keep feeding triage. */}
    {endpoints.state === 'ready' && hosts.length > 0 && <div className="panel ky-table-flush" hidden={matches.length === 0}>
      <table className="ky-table ky-responsive-table ky-fleet-table">
        <thead><tr><th>Endpoint</th><th>Status</th>{home && <><th>Running</th><th>Capacity</th><th>Reported</th></>}<th><span className="sr-only">Actions</span></th></tr></thead>
        <tbody>{hosts.map((e) => home && e.state === 'active'
          ? <EndpointRow key={e.id} org={org} endpoint={e} configure={configure} hidden={!match(e)} onReport={onReport} />
          : <tr key={e.id} hidden={!match(e)}><td data-label="Endpoint"><EndpointName org={org} e={e} /></td><td data-label="Status"><Status tone={endpointTone(e.state)}>{e.state}</Status></td>{home && <><td data-label="Running">—</td><td data-label="Capacity">—</td><td data-label="Reported">Disconnected</td></>}<td /></tr>)}
        </tbody>
      </table>
    </div>}
    {(offset > 0 || (endpoints.data?.length ?? 0) >= 20) && <div className="ky-pagination">
      <button className="btn-secondary" disabled={!offset} onClick={() => setOffset(offset - 20)}>Previous endpoints</button>
      <span>{endpoints.data?.length ? `Endpoints ${offset + 1}–${offset + endpoints.data.length}` : "No endpoints on this page"}</span>
      <button className="btn-secondary" disabled={endpoints.state !== 'ready' || (endpoints.data?.length ?? 0) < 20} onClick={() => setOffset(offset + 20)}>Next endpoints</button>
    </div>}
    {endpoints.state === 'ready' && activity.length > 0 && <section className="panel ky-triage" aria-labelledby="activity-heading">
      <h2 id="activity-heading" className="ky-table-title">Recent activity</h2>
      <ul className="ky-triage-list">{activity.map((a) => {
        const [word, tone] = Object.hasOwn(OUTCOMES, a.outcome) ? OUTCOMES[a.outcome] : ['Unrecognised', undefined];
        return <li key={a.id}>
          <Status tone={tone}>{word}</Status>
          <Link to={a.to}>{a.text}</Link>
          <span className="ky-triage-detail">{displayName(a.where)} · <time dateTime={a.at} title={new Date(a.at).toLocaleString()}>{ago(a.at, now)}</time></span>
        </li>;
      })}</ul>
    </section>}
  </>;
}

function EndpointName({ org, e }: { org: string; e: Endpoint }) {
  return <div className="ky-resource-name"><strong><Link to={endpointPath(org, e.id)}>{e.name}</Link></strong><span>{kind(e)}</span></div>;
}

// An active endpoint's row with its inventory summary; others are never read or polled. It
// reports its problems and newest commands to the fleet for triage.
function EndpointRow({ org, endpoint: e, configure, hidden, onReport }: { org: string; endpoint: Endpoint; configure: boolean; hidden: boolean; onReport: (id: string, report: Report | null) => void }) {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(e.id)}`;
  const inventory = useTenantResource<Inventory>(`${base}/inventory`, e.state);
  const commands = useTenantResource<Command[]>(`${base}/commands?limit=5`, e.state);
  const now = useNow();
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const timer = window.setInterval(() => { if (!document.hidden) { inventory.reload(); commands.reload(); } }, 30_000);
    return () => window.clearInterval(timer);
  }, [inventory.state === 'denied', inventory.reload, commands.reload]);
  const report = inventory.data;
  const live = !!(report && Number.isFinite(Date.parse(report.received_at)) && now - Date.parse(report.received_at) <= 180_000 && !inventory.refreshFailed);
  const snapshot = live ? report?.snapshot : undefined;
  const cluster = e.runtime === 'kubernetes';
  const kube = snapshot?.kubernetes;
  const containers = cluster ? [] : snapshot?.containers ?? [];
  useEffect(() => {
    const at = endpointPath(org, e.id);
    const problems: Problem[] = !report ? [] : !snapshot
      ? [{ key: e.id, tone: 'warning', word: 'Stale', name: e.name, to: at, detail: 'No fresh inventory for over three minutes' }]
      : [
        ...containers.filter(needsAttention).map((c): Problem => ({ key: `${e.id}/${c.id}`, tone: containerStatus(c).tone === 'danger' ? 'danger' : 'warning', word: containerStatus(c).text, name: c.name, to: containerPath(org, e.id, c.id), detail: `${e.name} · ${c.status}` })),
        ...(kube?.nodes.filter((n) => !n.ready).map((n): Problem => ({ key: `${e.id}/node/${n.name}`, tone: 'danger', word: 'Not ready', name: n.name, to: at, detail: `${e.name} · node` })) ?? []),
        ...(kube?.workloads.filter((w) => w.desired > 0 && w.ready < w.desired).map((w): Problem => ({ key: `${e.id}/${w.kind}/${w.namespace}/${w.name}`, tone: 'warning', word: 'Degraded', name: `${w.namespace}/${w.name}`, to: at, detail: `${e.name} · ${w.ready} of ${w.desired} ready` })) ?? []),
      ];
    const names = new Map((report?.snapshot.containers ?? []).map((c) => [c.id, c.name]));
    const activity = (Array.isArray(commands.data) ? commands.data : []).map((c): Activity => {
      const target = c.container_id ? names.get(c.container_id) ?? c.container_id.slice(0, 12) : c.reference ?? '';
      return { id: `${e.id}/${c.id}`, at: c.created_at, outcome: c.outcome, where: e.name, to: c.container_id && names.has(c.container_id) ? containerPath(org, e.id, c.container_id) : at,
        text: `${Object.hasOwn(ACTIONS, c.action) ? ACTIONS[c.action] : 'Command'}${target ? ` ${displayName(target)}` : ''}` };
    });
    onReport(e.id, { problems, activity });
  }, [org, e, report, live, commands.data, onReport]);
  useEffect(() => () => onReport(e.id, null), [e.id, onReport]);
  const unhealthy = containers.filter((c) => c.health === 'unhealthy').length;
  const exited = containers.filter((c) => c.state === 'exited' || c.state === 'dead').length;
  const notReady = kube?.nodes.filter((n) => !n.ready).length ?? 0;
  const running = !snapshot ? '—' : cluster
    ? kube ? `${kube.workloads.filter((w) => w.ready >= w.desired && w.desired > 0).length} / ${kube.workloads.length}` : '—'
    : `${containers.filter((c) => c.state === 'running').length} / ${containers.length}`;
  const reported = inventory.state !== 'ready' && inventory.state !== 'loading' ? <button className="btn-secondary" onClick={inventory.reload}>Retry</button>
    : !report ? '—'
    : !live ? <Status tone="warning" title="Inventory is stale or its refresh failed. Open the endpoint to check it.">Stale</Status>
    : <span title={new Date(report.received_at).toLocaleString()}>{ago(report.received_at, now)}{snapshot?.truncated?.length ? ' · incomplete' : ''}</span>;
  return <tr hidden={hidden}>
    <td data-label="Endpoint"><EndpointName org={org} e={e} /></td>
    <td data-label="Status"><Status tone={endpointTone(e.state)}>{e.state}</Status></td>
    <td data-label="Running" className="ky-num"><span className="ky-fleet-metric"><strong>{running}</strong> <span className="ky-muted">{cluster ? 'workloads ready' : 'running'}</span>
      {unhealthy > 0 && <Status tone="danger">{`${unhealthy} unhealthy`}</Status>}
      {exited > 0 && <Status tone="danger">{`${exited} exited`}</Status>}
      {notReady > 0 && <Status tone="danger">{`${notReady} ${notReady === 1 ? 'node' : 'nodes'} not ready`}</Status>}</span></td>
    <td data-label="Capacity" className="ky-num">{!snapshot ? '—' : cluster
      ? kube ? `${kube.nodes.length} ${kube.nodes.length === 1 ? 'node' : 'nodes'} · ${kube.pods.length} pods` : '—'
      : <>{snapshot.engine?.cpus || '—'} CPUs · <span>{snapshot.engine?.memory_bytes ? `${(snapshot.engine.memory_bytes / 2 ** 30).toFixed(1)} GiB` : '—'}</span></>}</td>
    <td data-label="Reported">{reported}</td>
    <td className="ky-row-actions">{configure && <Link className="btn btn-secondary" to={cluster ? workloadRunPath(org, e.id) : runPath(org, e.id)}>Run a container{cluster ? ' on Kubernetes' : ''}</Link>}</td>
  </tr>;
}
