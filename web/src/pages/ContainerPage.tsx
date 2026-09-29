import React, { lazy, Suspense, useEffect, useState } from 'react';
import { Box } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ContainerControls, ContainerLogs } from '../components/ContainerControls';
import { displayName } from '../components/Endpoints';
import { ContainerPorts } from '../components/ContainerPorts';
import { ago, attachments, bytes, healthBadge, stateBadge, uptime } from '../components/containerFacts';
import { parseInspection, type Inspection } from '../components/ApplicationInspection';
import { containerPath, endpointPath, navigate, useSearchParam } from '../router';
import { canExec, useTenantResource, type Container, type Endpoint, type Inventory, type MemberOrganization } from '../tenant';
const ContainerTerminal = lazy(() => import('../components/ContainerTerminal').then((m) => ({ default: m.ContainerTerminal })));

const TABS = ['overview', 'configuration', 'logs', 'terminal', 'activity'] as const;
type Tab = typeof TABS[number];
interface Command { id: string; action: string; outcome: string; detail?: string; created_at: string }
interface Rollup { hour: string; samples: number; cpu_avg: number; cpu_max: number; memory_avg: number; memory_max: number; rx_bytes: number; tx_bytes: number; pids_max: number; restart_count: number }

// Ticks once a second while mounted so uptime is live; the value is the clock, not the row.
function useNow() {
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
  return now;
}

// Folds every hourly row: averages weighted by samples, peaks as maxima, traffic summed.
function aggregate(rows: Rollup[]): Rollup | null {
  const samples = rows.reduce((n, r) => n + r.samples, 0);
  if (samples <= 0) return null;
  const weighted = (f: (r: Rollup) => number) => rows.reduce((n, r) => n + f(r) * r.samples, 0) / samples;
  const max = (f: (r: Rollup) => number) => Math.max(...rows.map(f));
  return {
    hour: '', samples,
    cpu_avg: weighted((r) => r.cpu_avg), cpu_max: max((r) => r.cpu_max),
    memory_avg: weighted((r) => r.memory_avg), memory_max: max((r) => r.memory_max),
    rx_bytes: rows.reduce((n, r) => n + r.rx_bytes, 0), tx_bytes: rows.reduce((n, r) => n + r.tx_bytes, 0),
    pids_max: max((r) => r.pids_max), restart_count: max((r) => r.restart_count),
  };
}

export const ContainerPage: React.FC<{ org: string; endpoint: string; container: string }> = ({ org, endpoint, container }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const details = useTenantResource<Endpoint>(base);
  const inventory = useTenantResource<Inventory>(`${base}/inventory`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const role = (organizations.data ?? []).find((o) => o.id === org)?.role;
  const exec = canExec(role);
  const requested = useSearchParam('tab');
  const tabs = TABS.filter((t) => t !== 'terminal' || exec);
  const tab: Tab = (tabs as readonly string[]).includes(requested) ? requested as Tab : 'overview';
  const [status, setStatus] = useState('');
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const t = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(t);
  }, [inventory.state, inventory.reload]);
  const e = details.data;
  const c = inventory.data?.snapshot.containers.find((row) => row.id === container) ?? null;
  const active = e?.state === 'active';
  const scope = `Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`;
  const health = c ? healthBadge(c.health) : null;
  return <div className="ky-page ky-container-page">
    <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link><span>/</span><Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link></nav>
    <div className="ky-page-heading">
      <h1 style={{ fontSize: 24 }}><Box size={24} style={{ color: 'var(--accent)' }} /><span>{c ? displayName(c.name) : container.slice(0, 12)}</span>
        {c && <span className={stateBadge(c.state)} title={c.status}>{displayName(c.state)}</span>}
        {health && <span className={`badge ${health.className}`}>{health.text}</span>}
      </h1>
      {c && <ContainerControls key={c.id} base={base} container={c} active={active} scope={scope} onRefresh={inventory.reload} canExec={exec} org={org} endpoint={endpoint} onStatus={setStatus} />}
    </div>
    {c && <p style={{ color: 'var(--ink)' }} title={c.image}>{displayName(c.image)}</p>}
    {status && <p role="status">{status}</p>}
    <StateNotice state={details.state} onRetry={details.reload} />
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && !c && <EmptyNotice>This container is no longer reported by <Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link>. It may have been removed or renamed.</EmptyNotice>}
    {c && <>
      <nav aria-label="Container sections" className="ky-resource-tabs">
        {tabs.map((t) => <button type="button" key={t} aria-pressed={tab === t} onClick={() => navigate(containerPath(org, endpoint, container, t === 'overview' ? undefined : t))}>{t[0].toUpperCase() + t.slice(1)}</button>)}
      </nav>
      {tab === 'overview' && <Overview base={base} container={c} received={inventory.data?.received_at ?? ''} />}
      {tab === 'configuration' && <Configuration base={base} container={c} capable={(e?.capabilities ?? []).includes('container.inspect')} />}
      {tab === 'logs' && <section className="panel" aria-label="Logs"><ContainerLogs key={c.id} url={`${base}/containers/${encodeURIComponent(c.id)}/logs`} name={c.name} /></section>}
      {tab === 'terminal' && exec && <section className="panel" aria-label="Terminal">{c.state === 'running' && active ? <Suspense fallback={<p role="status">Loading terminal…</p>}><ContainerTerminal key={`${base}/${c.id}/${c.image_id}`} base={base} container={c} scope={scope} /></Suspense> : <EmptyNotice>The terminal needs a running container on an active host.</EmptyNotice>}</section>}
      {tab === 'activity' && <Activity base={base} container={c.id} />}
    </>}
  </div>;
};

function Overview({ base, container: c, received }: { base: string; container: Container; received: string }) {
  const now = useNow();
  const rollups = useTenantResource<Rollup[]>(`${base}/containers/${encodeURIComponent(c.id)}/rollups?hours=24`);
  const up = uptime(c.started_at, now);
  const nets = attachments(c);
  const usage = Array.isArray(rollups.data) ? aggregate(rollups.data) : null;
  return <section className="panel" aria-label="Overview">
    <dl className="ky-facts">
      <dt>State</dt><dd>{displayName(c.state)}{c.status ? ` · ${displayName(c.status)}` : ''}</dd>
      <dt>Uptime</dt><dd>{up || '—'}</dd>
      <dt>Started</dt><dd>{up ? new Date(c.started_at!).toLocaleString() : '—'}</dd>
      <dt>Created</dt><dd>{c.created_at ? new Date(c.created_at).toLocaleString() : '—'}</dd>
      <dt>Image</dt><dd><span title={c.image}>{displayName(c.image)}</span><br /><small className="font-mono">{c.image_id}</small></dd>
      <dt>Restart policy</dt><dd>{c.restart_policy ? displayName(c.restart_policy) : '—'}</dd>
      <dt>Networks</dt><dd>{nets.length ? <ul className="ky-list">{nets.map((n) => <li key={n.name}>{displayName(n.name)}{n.ip ? ` · ${n.ip}` : ''}{n.ip6 ? ` · ${n.ip6}` : ''}</li>)}</ul> : '—'}</dd>
      <dt>Ports</dt><dd><ContainerPorts ports={c.ports} /></dd>
      <dt>Mounts</dt><dd>{c.mounts?.length ? <ul className="ky-list">{c.mounts.map((m) => <li key={m.target}>{displayName(m.kind)} {displayName(m.source)} → {displayName(m.target)}{m.read_only ? ' (read-only)' : ''}</li>)}</ul> : '—'}{c.mounts_truncated ? ' (list truncated)' : ''}</dd>
      <dt>Labels</dt><dd>{Object.keys(c.labels).length ? <ul className="ky-list">{Object.entries(c.labels).map(([k, v]) => <li key={k} style={{ overflowWrap: 'anywhere' }}>{displayName(k)}={displayName(v)}</li>)}</ul> : '—'}</dd>
      {c.compose_project && <><dt>Compose project</dt><dd>{displayName(c.compose_project)}</dd></>}
      <dt>Container ID</dt><dd className="font-mono" style={{ fontSize: 11 }}>{c.id}</dd>
      <dt>Reported</dt><dd>{received ? ago(received, now) : '—'}</dd>
    </dl>
    <h2 style={{ fontSize: 16, marginTop: 16 }}>Last 24 hours</h2>
    <StateNotice state={rollups.state} onRetry={rollups.reload} />
    {rollups.state === 'ready' && (usage ? <dl className="ky-facts">
      <dt>CPU</dt><dd>avg {usage.cpu_avg.toFixed(1)}% · peak {usage.cpu_max.toFixed(1)}%</dd>
      <dt>Memory</dt><dd>avg {bytes(usage.memory_avg)} · peak {bytes(usage.memory_max)}</dd>
      <dt>Network</dt><dd>rx {bytes(usage.rx_bytes)} · tx {bytes(usage.tx_bytes)}</dd>
      <dt>Processes</dt><dd>peak {usage.pids_max}</dd>
      <dt>Restarts</dt><dd>{usage.restart_count}</dd>
    </dl> : <EmptyNotice>No usage samples yet.</EmptyNotice>)}
  </section>;
}

function Configuration({ base, container: c, capable }: { base: string; container: Container; capable: boolean }) {
  const [result, setResult] = useState<{ kind: 'loading' } | { kind: 'ready'; data: Inspection } | { kind: 'error'; text: string }>({ kind: 'loading' });
  useEffect(() => {
    if (!capable) return;
    const controller = new AbortController();
    (async () => {
      try {
        const r = await fetch(`${base}/containers/${encodeURIComponent(c.id)}/inspection`, { signal: controller.signal, cache: 'no-store' });
        if (controller.signal.aborted) return;
        if (!r.ok) { setResult({ kind: 'error', text: r.status === 403 ? 'You do not have permission to inspect this container.' : r.status === 429 ? 'Inspection capacity reached. Try again later.' : 'Inspection is unavailable right now.' }); return; }
        const data = parseInspection(await r.json(), { container_id: c.id, image_id: c.image_id, created_unix: Math.floor(Date.parse(c.created_at) / 1000) });
        if (!controller.signal.aborted) setResult(data ? { kind: 'ready', data } : { kind: 'error', text: 'The inspection did not match this container.' });
      } catch { if (!controller.signal.aborted) setResult({ kind: 'error', text: 'The inspection connection was lost.' }); }
    })();
    return () => controller.abort();
  }, [base, c.id, c.image_id, c.created_at, capable]);
  return <section className="panel" aria-label="Configuration">
    <p>Read-only observation of the running container. Environment values, commands and paths are not shown here.</p>
    {!capable && <EmptyNotice>Upgrade the host agent to enable live inspection.</EmptyNotice>}
    {capable && result.kind === 'loading' && <p role="status">Inspecting container…</p>}
    {capable && result.kind === 'error' && <p role="status">{result.text}</p>}
    {capable && result.kind === 'ready' && <dl className="ky-facts">
      <dt>Restart policy</dt><dd>{displayName(result.data.restart_policy)}{result.data.restart_retries ? ` (${result.data.restart_retries} retries)` : ''}</dd>
      <dt>Network mode</dt><dd>{displayName(result.data.network_mode)} · {result.data.network_count} networks</dd>
      <dt>Mounts</dt><dd>{result.data.mounts.volume} volumes · {result.data.mounts.bind} binds · {result.data.mounts.tmpfs} tmpfs · {result.data.mounts.read_only} read-only</dd>
      <dt>Platform</dt><dd>{result.data.image_platform.os}/{result.data.image_platform.architecture}</dd>
      <dt>Flags</dt><dd>{[result.data.privileged && 'privileged', result.data.read_only_rootfs && 'read-only root', result.data.auto_remove && 'auto-remove'].filter(Boolean).join(', ') || 'none'}</dd>
      <dt>Observed</dt><dd>{new Date(result.data.observed_at).toLocaleString()}</dd>
    </dl>}
  </section>;
}

function Activity({ base, container }: { base: string; container: string }) {
  const commands = useTenantResource<Command[]>(`${base}/commands?container=${encodeURIComponent(container)}&limit=50`);
  return <section className="panel" aria-label="Activity">
    <div className="panel-header"><h2>Recent activity</h2><button className="btn-secondary" onClick={commands.reload}>Refresh</button></div>
    <StateNotice state={commands.state} onRetry={commands.reload} />
    {commands.state === 'ready' && Array.isArray(commands.data) && (commands.data.length ? <ul className="ky-list">{commands.data.map((k) => <li key={k.id}>{new Date(k.created_at).toLocaleString()} · {k.action} · {k.outcome || 'pending'}{k.detail ? ` — ${displayName(k.detail)}` : ''}</li>)}</ul> : <EmptyNotice>No commands have been sent to this container.</EmptyNotice>)}
  </section>;
}
