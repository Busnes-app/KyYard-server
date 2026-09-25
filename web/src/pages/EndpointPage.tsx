import { ContainerPorts } from '../components/ContainerPorts';
import type { ApplicationInstance } from '../components/ApplicationAdoption';
import React, { useState } from 'react';
import { usePagination } from '../components/Pagination';
import { ComposeProjects } from '../components/ComposeProjects';
import { ImageControls } from '../components/ImageControls';
import { ContainerControls } from '../components/ContainerControls';
import { Server } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { envPath } from '../router';
import { canExec, useTenantResource, type Endpoint, type Inventory, type MemberOrganization, type Sample } from '../tenant';
import { displayName } from '../components/Endpoints';

const bytes = (n: number) => n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GiB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(0)} MiB` : `${n} B`;
const ago = (iso: string) => { const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000)); return s < 90 ? `${s}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`; };

// Freshness is shown from received_at (server clock); observed_at is the agent's clock and is
// flagged when it disagrees by more than five minutes.
export const EndpointPage: React.FC<{ org: string; endpoint: string }> = ({ org, endpoint }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const [view, setView] = useState('containers');
  const [search, setSearch] = useState('');
  const [projectFilter, setProjectFilter] = useState<{ base: string; name: string } | null>(null);
  const details = useTenantResource<Endpoint>(base);
  const inventory = useTenantResource<Inventory>(`${base}/inventory`);
  const commands = useTenantResource<{ id: string; action: string; outcome: string; detail?: string; container_id?: string; reference?: string }[]>(`${base}/commands?limit=20`);
  const ownership = useTenantResource<ApplicationInstance[]>(`${base}/applications`);
  const samples = useTenantResource<Sample[]>(`${base}/samples`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const exec = canExec((Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role);
  const latest = new Map((Array.isArray(samples.data) ? samples.data : []).map((s) => [s.container_id, s]));
  // -1 is "no interval yet" and a missing row is "no data"; neither is zero usage.
  const usage = (c: { id: string; state: string }) => {
    const s = latest.get(c.id);
    if (!s) return c.state === 'running' ? 'no data' : '—';
    const cpu = s.cpu_percent < 0 ? 'cpu —' : `cpu ${s.cpu_percent.toFixed(1)}%`;
    // A runtime that did not answer reports -1, and a server too old to send the field at all
    // reports nothing; neither is a container that has never restarted, so both stay silent.
    const restarts = typeof s.restart_count === 'number' && s.restart_count >= 0 ? ` · ${s.restart_count} restarts` : '';
    return `${cpu} · mem ${bytes(s.memory_bytes)}${restarts}`;
  };
  const e = details.data;
  const inv = inventory.data;
  // A filter belongs to one endpoint and must not hide a refreshed or different host.
  const selectedProject = projectFilter?.base === base && inv?.snapshot.containers.some((c) => c.compose_project === projectFilter.name) ? projectFilter.name : null;
  const visibleContainers = inv?.snapshot.containers.filter((c) => selectedProject === null || c.compose_project === selectedProject).filter((c) => `${c.name} ${c.image}`.toLowerCase().includes(search.toLowerCase())) ?? [];
  const skew = inv ? Math.abs(new Date(inv.received_at).getTime() - new Date(inv.observed_at).getTime()) > 5 * 60 * 1000 : false;
  const stale = inv ? Date.now() - new Date(inv.received_at).getTime() > 3 * 60 * 1000 : false;

  return (
    <div className="ky-page ky-endpoint-page">
      <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link>{e && <><span>/</span><Link to={envPath(org, e.environment_id)}>Environment</Link></>}</nav>
      <div className="ky-page-heading">
      <h1 style={{ fontSize: 24, display: 'flex', alignItems: 'center', gap: 10 }}>
        <Server size={24} style={{ color: 'var(--accent)' }} /><span>{e?.name ?? endpoint}</span>
        {e && <span className={`badge ${e.state === 'active' ? 'badge-success' : e.state === 'pending' ? 'badge-accent' : 'badge-danger'}`}>{e.state}</span>}
      </h1>
      <button className="btn-secondary" onClick={() => { details.reload(); inventory.reload(); samples.reload(); commands.reload(); ownership.reload(); }}>Refresh inventory</button>
      </div>
      <nav aria-label="Host resources" className="ky-resource-tabs">
        {['containers', 'projects', 'images', 'networks', 'volumes', 'activity', 'details'].map((item) => <button type="button" key={item} aria-pressed={view === item} onClick={() => setView(item)}>{item[0].toUpperCase() + item.slice(1)}</button>)}
      </nav>
      <StateNotice state={details.state} onRetry={() => { details.reload(); inventory.reload(); }} />
      {view === 'details' && details.state === 'ready' && e && (
        <section className="panel"><h2>Host details & identity</h2>
          <div className="panel-header"><h2 style={{ fontSize: 16 }}>Endpoint</h2></div>
          <dl className="ky-facts">
            <dt>Runtime</dt><dd>{e.runtime} {inv?.snapshot.engine.version ?? e.facts.runtime_version ?? ''}</dd>
            <dt>Host</dt><dd>{inv?.snapshot.engine.hostname ?? e.facts.hostname ?? '—'} · {inv?.snapshot.engine.os ?? e.facts.os ?? ''} {inv?.snapshot.engine.arch ?? ''}</dd>
            <dt>Capacity</dt><dd>{inv ? `${inv.snapshot.engine.cpus} CPUs · ${bytes(inv.snapshot.engine.memory_bytes)}` : 'not reported yet'}</dd>
            <dt>Key</dt><dd className="font-mono" style={{ fontSize: 11 }}>{e.fingerprint || '—'}</dd>
            <dt>Capabilities</dt><dd>{e.capabilities.length ? e.capabilities.join(', ') : 'none reported'}</dd>
          </dl>
        </section>
      )}
      {view === 'activity' && <section className="panel"><div className="panel-header"><h2>Recent activity</h2></div><button className="btn-secondary" onClick={commands.reload}>Refresh activity</button><StateNotice state={commands.state} onRetry={commands.reload} />{Array.isArray(commands.data) && <ul className="ky-list">{commands.data.map((c) => <li key={c.id}>{c.action} · {c.container_id || c.reference} · {c.outcome || 'pending'}{c.detail ? ` — ${c.detail}` : ''}</li>)}</ul>}{commands.state === 'ready' && Array.isArray(commands.data) && commands.data.length === 0 && <EmptyNotice>No recent activity on this host.</EmptyNotice>}</section>}
      {inventory.state === 'notfound' && details.state === 'ready' && <EmptyNotice>No inventory yet. It arrives with the agent's first report after approval.</EmptyNotice>}
      {inventory.state !== 'notfound' && <StateNotice state={inventory.state} onRetry={inventory.reload} />}
      {inventory.state === 'ready' && inv && (
        <>
          <p role="status" style={{ color: stale ? 'var(--danger)' : 'var(--ink)', fontSize: 13 }}>
            Inventory generation {inv.generation}, received {ago(inv.received_at)}{stale ? ' (stale: no report for over three minutes)' : ''}{skew ? ' · agent clock differs from the server by more than five minutes' : ''}.
            {inv.snapshot.truncated?.length ? ` Lists truncated: ${inv.snapshot.truncated.join(', ')}.` : ''}
          </p>
          {view === 'projects' && <><StateNotice state={ownership.state} onRetry={ownership.reload} /><ComposeProjects ownership={ownership.state === 'ready' && Array.isArray(ownership.data) ? ownership.data : null} containers={inv.snapshot.containers} truncated={inv.snapshot.truncated?.includes('containers') ?? false} onSelect={(name) => {
            setProjectFilter({ base, name }); setView('containers');
            requestAnimationFrame(() => document.getElementById('endpoint-containers')?.focus());
          }} /></>}
          {view === 'containers' && <div id="endpoint-containers" tabIndex={-1}>
          <div className="ky-toolbar"><input type="search" aria-label="Find containers" placeholder="Search containers or images" value={search} onChange={(event) => setSearch(event.target.value)} /><span>{visibleContainers.length} containers</span></div>
          {selectedProject !== null && <p>Showing containers for <strong style={{ overflowWrap: 'anywhere' }}><bdi>{selectedProject}</bdi></strong>. <button className="btn-secondary" onClick={() => setProjectFilter(null)}>Show all containers</button></p>}
          <Table key={JSON.stringify([base, search, selectedProject])} title="Containers" rows={visibleContainers} empty={search ? "No matching containers." : "No containers on this host."} head={['Container', 'Status', 'Usage', 'Actions']} render={(c) => [<div className="ky-resource-name"><strong>{displayName(c.name)}</strong><span title={c.image}>{displayName(c.image)}</span><ContainerPorts ports={c.ports} />{c.compose_project && <small>{displayName(c.compose_project)}</small>}</div>, <span className={`badge ${c.state === 'running' ? 'badge-success' : c.state === 'exited' || c.state === 'dead' ? 'badge-danger' : 'badge-secondary'}`} title={c.status}>{displayName(c.state)}</span>, usage(c), <ContainerControls key={c.id} base={base} container={c} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onRefresh={commands.reload} canExec={exec} />]} />
          </div>}
          {view === 'images' && <><section className="panel"><h2>Pull an image</h2><ImageControls key={base} kind="pull" base={base} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onActivity={commands.reload} /><p>Use an explicit tag or digest. A pull downloads an image; it does not update running containers.</p></section>
          <Table title="Images" rows={inv.snapshot.images} empty="No images on this host." head={['Tags', 'Size', 'ID', 'Actions']} render={(i) => [i.tags.map(displayName).join(', ') || '<untagged>', bytes(i.size_bytes), <span title={i.id}>{i.id.slice(0, 19)}</span>, <ImageControls key={`${base}/${i.id}`} kind="remove" imageID={i.id} base={base} active={e?.state === 'active'} scope={`Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`} onActivity={commands.reload} />]} />
          </>}
          {view === 'networks' && <Table title="Networks" rows={inv.snapshot.networks} empty="No networks." head={['Name', 'Driver', 'Scope']} render={(n) => [displayName(n.name), displayName(n.driver), displayName(n.scope)]} />}
          {view === 'volumes' && <Table title="Volumes" rows={inv.snapshot.volumes} empty="No volumes." head={['Name', 'Driver', 'Mountpoint']} render={(v) => [displayName(v.name), displayName(v.driver), displayName(v.mountpoint)]} />}
        </>
      )}
    </div>
  );
};

function Table<T>({ title, rows, empty, head, render }: { title: string; rows: T[]; empty: string; head: string[]; render: (r: T) => React.ReactNode[] }) {
  const pagination = usePagination(rows, title);
  return (
    <section className="panel">
      <div className="panel-header"><h2 style={{ fontSize: 16 }}>{title} <span style={{ color: 'var(--ink)', fontWeight: 400 }}>({rows.length})</span></h2></div>
      {pagination.controls}
      {rows.length === 0 ? <EmptyNotice>{empty}</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr>{head.map((h) => <th key={h}>{h}</th>)}</tr></thead>
            <tbody>{pagination.rows.map((r, i) => <tr key={i}>{render(r).map((cell, j) => <td key={j} data-label={head[j]}>{cell}</td>)}</tr>)}</tbody>
          </table>
        </div>
      )}
    </section>
  );
}
