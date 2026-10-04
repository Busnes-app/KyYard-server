import { useContainerImageUpdate } from '../components/useContainerImageUpdate';
import { RunYAML } from '../components/RunYAML';
import React, { lazy, Suspense, useEffect, useState } from 'react';
import { Box } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ContainerControls, ContainerLogs, useCommand } from '../components/ContainerControls';
import { displayName } from '../components/Endpoints';
import { ContainerPorts } from '../components/ContainerPorts';
import { ago, attachments, bytes, healthBadge, stateBadge, uptime, useNow } from '../components/containerFacts';
import { parseInspection, type Inspection } from '../components/ApplicationInspection';
import { CommandResult, ContainerConfigurationForm, newContainer } from '../components/ContainerConfigurationForm';
import { parseConfiguration } from '../components/containerConfiguration';
import type { ApplicationInstance } from '../components/ApplicationAdoption';
import { containerPath, endpointPath, envPath, navigate, useSearchParam } from '../router';
import { canConfigure, canExec, canRunContainers, useTenantResource, type Container, type ContainerConfiguration, type DirectCommand, type Endpoint, type Inventory, type MemberOrganization } from '../tenant';
const ContainerTerminal = lazy(() => import('../components/ContainerTerminal').then((m) => ({ default: m.ContainerTerminal })));

const TABS = ['overview', 'configuration', 'logs', 'terminal', 'activity'] as const;
type Tab = typeof TABS[number];
interface Command { id: string; action: string; outcome: string; detail?: string; created_at: string }
interface Rollup { hour: string; samples: number; cpu_avg: number; cpu_max: number; memory_avg: number; memory_max: number; rx_bytes: number; tx_bytes: number; pids_max: number; restart_count: number }

interface Usage { samples: number; cpu_avg: number | null; cpu_max: number | null; memory_avg: number; memory_max: number; rx_bytes: number; tx_bytes: number; pids_max: number; restart_count: number | null }

// Folds the hourly rows. A negative CPU or restart value means "not measured" and is skipped.
// rx/tx are each hour's highest cumulative counter, so traffic is the sum of hour-to-hour rises
// (a counter reset adds nothing, a single hour adds nothing).
function aggregate(rows: Rollup[]): Usage | null {
  const samples = rows.reduce((n, r) => n + r.samples, 0);
  if (samples <= 0) return null;
  const sorted = [...rows].sort((a, b) => Date.parse(a.hour) - Date.parse(b.hour));
  const max = (f: (r: Rollup) => number) => Math.max(...rows.map(f));
  const rise = (f: (r: Rollup) => number) => sorted.slice(1).reduce((n, r, i) => n + Math.max(0, f(r) - f(sorted[i])), 0);
  const measured = rows.filter((r) => r.cpu_avg >= 0);
  const cpuSamples = measured.reduce((n, r) => n + r.samples, 0);
  const peaks = rows.map((r) => r.cpu_max).filter((v) => v >= 0);
  const restarts = rows.map((r) => r.restart_count).filter((v) => v >= 0);
  return {
    samples,
    cpu_avg: cpuSamples > 0 ? measured.reduce((n, r) => n + r.cpu_avg * r.samples, 0) / cpuSamples : null,
    cpu_max: peaks.length ? Math.max(...peaks) : null,
    memory_avg: rows.reduce((n, r) => n + r.memory_avg * r.samples, 0) / samples, memory_max: max((r) => r.memory_max),
    rx_bytes: rise((r) => r.rx_bytes), tx_bytes: rise((r) => r.tx_bytes),
    pids_max: max((r) => r.pids_max), restart_count: restarts.length ? Math.max(...restarts) : null,
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
  // The page, not the form, polls a sent recreate: switching tabs or the inventory dropping the
  // replaced container unmounts the form, but the operator still needs the steps and the new one.
  const [sent, setSent] = useState<{ base: string; container: string; command: DirectCommand } | null>(null);
  const scopedSent = sent?.base === base && sent.container === container ? sent.command : null;
  const record = (command: DirectCommand) => setSent({ base, container, command });
  const { command, error: pollError } = useCommand(base, scopedSent, inventory.reload);
  const imageUpdate = useContainerImageUpdate(base, (cmd) => { record(cmd); if (cmd.outcome) inventory.reload(); });
  const settled = command?.outcome ? command : null;
  const created = settled ? newContainer(settled, container) : '';
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const t = window.setInterval(() => { if (!document.hidden) { inventory.reload(); details.reload(); } }, 30_000);
    return () => window.clearInterval(t);
  }, [inventory.state, inventory.reload, details.reload]);
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
    {imageUpdate.busy && <p role="status">Reading current settings and submitting the image update…</p>}
    {imageUpdate.error && <p role="alert" className="dr-alert dr-alert-error">{imageUpdate.error}</p>}
    <StateNotice state={details.state} onRetry={details.reload} />
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && !c && <EmptyNotice>This container is no longer reported by <Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link>. It may have been removed or renamed.{created && <> <Link to={containerPath(org, endpoint, created, 'configuration')}>Open the new container</Link></>}</EmptyNotice>}
    {command && <section className="panel" aria-label="Last change"><SentCommand command={command} error={pollError} org={org} endpoint={endpoint} current={container} link={!!c} /></section>}
    {c && <>
      <nav aria-label="Container sections" className="ky-resource-tabs">
        {tabs.map((t) => <button type="button" key={t} aria-pressed={tab === t} onClick={() => navigate(containerPath(org, endpoint, container, t === 'overview' ? undefined : t))}>{t[0].toUpperCase() + t.slice(1)}</button>)}
      </nav>
      {tab === 'overview' && <Overview base={base} container={c} received={inventory.data?.received_at ?? ''} />}
      {tab === 'configuration' && (organizations.state === 'loading' || details.state === 'loading' ? <p role="status">Loading…</p> : !e ? null
        : canConfigure(role) ? <EditConfiguration key={`${c.id}/${c.image_id}/${c.created_at}`} base={base} org={org} endpoint={e} container={c} onUpdateImage={() => void imageUpdate.update(c)} pending={imageUpdate.busy || imageUpdate.lost || (!!command && (!command.outcome || command.outcome === 'unknown' || command.outcome === 'succeeded'))} onSent={(cmd) => { record(cmd); if (cmd.outcome) inventory.reload(); }} />
        : <Configuration base={base} container={c} capable={e.capabilities.includes('container.inspect')} />)}
      {tab === 'logs' && <section className="panel" aria-label="Logs"><ContainerLogs key={c.id} url={`${base}/containers/${encodeURIComponent(c.id)}/logs`} name={c.name} /></section>}
      {tab === 'terminal' && exec && <section className="panel" aria-label="Terminal">{c.state === 'running' && active ? <Suspense fallback={<p role="status">Loading terminal…</p>}><ContainerTerminal key={`${base}/${c.id}/${c.image_id}`} base={base} container={c} scope={scope} /></Suspense> : <EmptyNotice>The terminal needs a running container on an active host.</EmptyNotice>}</section>}
      {tab === 'activity' && <Activity base={base} container={c.id} />}
    </>}
  </div>;
};

export const ContainerRunPage: React.FC<{ org: string; endpoint: string }> = ({ org, endpoint }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const details = useTenantResource<Endpoint>(base);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const role = (Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role;
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const { command, error } = useCommand(base, sent);
  const [format, setFormat] = useState('form');
  const [started, setStarted] = useState(false);
  return <div className="ky-page">
    <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link><span>/</span><Link to={endpointPath(org, endpoint)}>{details.data?.name ?? endpoint}</Link></nav>
    <h1 style={{ fontSize: 24 }}>Run a container</h1>
    <StateNotice state={details.state} onRetry={details.reload} />
    {organizations.state === 'loading' ? <p role="status">Loading…</p>
      : organizations.state !== 'ready' ? <StateNotice state={organizations.state} onRetry={organizations.reload} />
      : !canConfigure(role) ? <EmptyNotice>Only an organization administrator can run containers.</EmptyNotice>
      : details.state !== 'ready' || !details.data ? null
      : details.data.runtime !== 'docker' ? <EmptyNotice>Containers can be run only on a Docker host.</EmptyNotice>
      : !canRunContainers(details.data) ? <EmptyNotice>Upgrade the host agent to run containers here.</EmptyNotice>
      : <section className="panel"><div className="ky-toolbar"><button className="btn-secondary" aria-pressed={format === 'form'} disabled={started || !!sent} onClick={() => setFormat('form')}>Container form</button><button className="btn-secondary" aria-pressed={format === 'yaml'} disabled={started || !!sent} onClick={() => setFormat('yaml')}>Docker Compose YAML</button></div>{format === 'yaml' ? <RunYAML onStarted={() => setStarted(true)} base={base} runtime="docker" org={org} endpoint={endpoint} /> : <ContainerConfigurationForm onStarted={() => setStarted(true)} base={base} mode="run" pending={!!command && (!command.outcome || command.outcome === 'succeeded')} onSent={setSent} />}
        {command && <SentCommand command={command} error={error} org={org} endpoint={endpoint} current="" />}</section>}
  </div>;
};

// SentCommand is a sent recreate or run: waiting, a failed poll, or the settled result.
function SentCommand({ command, error, ...rest }: { command: DirectCommand; error: string; org: string; endpoint: string; current: string; link?: boolean }) {
  if (command.outcome) return <CommandResult command={command} {...rest} />;
  return error ? <p role="alert" className="dr-alert dr-alert-error">Could not read the command result. Check recent activity before trying again.</p>
    : <p role="status">Command sent; waiting for the host. Do not retry while its outcome is unknown.</p>;
}

const READ_ERRORS: Record<number, string> = {
  401: 'Your session has expired. Sign in again.',
  403: 'You do not have permission to edit this container.',
  429: 'Too many configuration requests. Wait a minute and try again.',
  501: 'Upgrade the host agent to enable editing.',
  504: 'The host did not answer in time. Try again.',
};
const READ_CONFLICTS: Record<string, string> = {
  application_managed: 'An application manages this container. Change it through the application.',
  runtime_unsupported: 'Only a Docker host supports editing containers.',
  endpoint_offline: 'The host is not connected. Reconnect it to edit this container.',
};

// EditConfiguration reads the full configuration (environment values included) once per mount;
// a container an adopted application owns is never read: it shows the redacted view and edits
// through its application.
function EditConfiguration({ base, org, endpoint, container: c, pending, onSent, onUpdateImage }: { base: string; org: string; endpoint: Endpoint; container: Container; pending: boolean; onSent: (cmd: DirectCommand) => void; onUpdateImage: () => void }) {
  const capable = endpoint.capabilities.includes('container.configure');
  const owners = useTenantResource<ApplicationInstance[]>(`${base}/applications`);
  const owner = owners.state === 'ready' && Array.isArray(owners.data) ? owners.data.find((i) => i.containers?.some((x) => x.id === c.id)) : undefined;
  const [attempt, setAttempt] = useState(0);
  const [read, setRead] = useState<{ kind: 'loading' } | { kind: 'ready'; data: ContainerConfiguration } | { kind: 'error'; text: string }>({ kind: 'loading' });
  const settled = owners.state !== 'loading';
  useEffect(() => {
    if (!capable || !settled || owner) return;
    const controller = new AbortController();
    setRead({ kind: 'loading' });
    (async () => {
      try {
        const r = await fetch(`${base}/containers/${encodeURIComponent(c.id)}/configuration`, { signal: controller.signal, cache: 'no-store' });
        if (controller.signal.aborted) return;
        if (!r.ok) {
          const code = r.status === 409 ? ((await r.json().catch(() => ({}))) as { code?: unknown }).code : undefined;
          if (controller.signal.aborted) return;
          setRead({ kind: 'error', text: typeof code === 'string' && Object.hasOwn(READ_CONFLICTS, code) ? READ_CONFLICTS[code] ?? '' : READ_ERRORS[r.status] ?? 'The configuration is unavailable right now.' });
          return;
        }
        const data = parseConfiguration(await r.json(), { container_id: c.id, image_id: c.image_id, created_unix: Math.floor(Date.parse(c.created_at) / 1000) });
        if (!controller.signal.aborted) setRead(data ? { kind: 'ready', data } : { kind: 'error', text: 'The configuration did not match this container. Refresh the inventory.' });
      } catch { if (!controller.signal.aborted) setRead({ kind: 'error', text: 'The configuration connection was lost.' }); }
    })();
    return () => controller.abort();
  }, [base, c.id, c.image_id, c.created_at, capable, settled, owner, attempt]);
  const redacted = <Configuration base={base} container={c} capable={endpoint.capabilities.includes('container.inspect')} />;
  if (!capable) return <><EmptyNotice>Upgrade the host agent to enable editing.</EmptyNotice>{redacted}</>;
  if (owner) return <><p className="dr-alert dr-alert-warn">Managed by application {displayName(owner.project)}. <Link to={envPath(org, endpoint.environment_id)}>Edit it there.</Link></p>{redacted}</>;
  return <section className="panel" aria-label="Configuration">
    {!settled || read.kind === 'loading' ? <p role="status">Reading configuration…</p>
      : read.kind === 'error' ? <><p role="status">{read.text}</p><button type="button" className="btn-secondary" onClick={() => setAttempt((n) => n + 1)}>Read again</button></>
      : <ContainerConfigurationForm base={base} mode="edit" container={c} initial={read.data} pending={pending} onSent={onSent} onUpdateImage={canRunContainers(endpoint) ? onUpdateImage : undefined} />}
  </section>;
}

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
      <dt>CPU</dt><dd>{usage.cpu_avg === null ? '—' : `avg ${usage.cpu_avg.toFixed(1)}% · peak ${usage.cpu_max === null ? '—' : `${usage.cpu_max.toFixed(1)}%`}`}</dd>
      <dt>Memory</dt><dd>avg {bytes(usage.memory_avg)} · peak {bytes(usage.memory_max)}</dd>
      <dt>Network</dt><dd>rx {bytes(usage.rx_bytes)} · tx {bytes(usage.tx_bytes)}</dd>
      <dt>Processes</dt><dd>peak {usage.pids_max}</dd>
      <dt>Restart count</dt><dd>{usage.restart_count ?? '—'}</dd>
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
