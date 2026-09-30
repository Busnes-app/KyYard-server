import React, { lazy, Suspense, useEffect, useState, type ReactNode } from 'react';
import { Layers } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ContainerLogs, useCommand } from '../components/ContainerControls';
import { displayName } from '../components/Endpoints';
import { uptime, useNow } from '../components/containerFacts';
import { ResourceTable } from '../components/ResourceTable';
import { WorkloadControls } from '../components/WorkloadControls';
import { PodControls } from '../components/PodControls';
import { EditWorkload, WorkloadResult } from '../components/WorkloadConfigurationForm';
import { commandLine, UPGRADE_CLUSTER } from '../components/workloadTexts';
import type { ApplicationInstance } from '../components/ApplicationAdoption';
import { endpointPath, envPath, navigate, useSearchParam, workloadPath } from '../router';
import { canConfigure, canExec, useTenantResource, type DirectCommand, type Endpoint, type Inventory, type MemberOrganization, type Pod, type Workload } from '../tenant';
const PodTerminal = lazy(() => import('../components/PodTerminal').then((m) => ({ default: m.PodTerminal })));

const TABS = ['overview', 'configuration', 'logs', 'terminal', 'activity'] as const;
type Tab = typeof TABS[number];
interface Command { id: string; action: string; outcome: string; detail?: string; created_at: string }

// WorkloadPage is one Deployment, StatefulSet or DaemonSet of a cluster endpoint, found in the
// stored inventory by namespace, lower-case kind and name. Its pods are the ones it owns.
export const WorkloadPage: React.FC<{ org: string; endpoint: string; namespace: string; kind: string; workload: string }> = ({ org, endpoint, namespace, kind, workload }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const details = useTenantResource<Endpoint>(base);
  const inventory = useTenantResource<Inventory>(`${base}/inventory`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const applications = useTenantResource<ApplicationInstance[]>(`${base}/applications`);
  const role = (Array.isArray(organizations.data) ? organizations.data : []).find((o) => o.id === org)?.role;
  const exec = canExec(role);
  const requested = useSearchParam('tab');
  const podParam = useSearchParam('pod');
  const tabs = TABS.filter((t) => t !== 'terminal' || exec);
  const tab: Tab = (tabs as readonly string[]).includes(requested) ? requested as Tab : 'overview';
  const [status, setStatus] = useState('');
  // The page, not the form, polls a sent apply, so its result survives tab switches.
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const { command, error: pollError } = useCommand(base, sent, inventory.reload);
  const settled = command?.outcome ? command : null;
  const [settledId, setSettledId] = useState('');
  useEffect(() => { if (settled) setSettledId(settled.id); }, [settled?.id]);
  const conflict = !!settled?.result?.steps.some((s) => s.code === 'conflict');
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const t = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(t);
  }, [inventory.state, inventory.reload]);
  const e = details.data;
  const kube = inventory.data?.snapshot.kubernetes;
  const w = kube?.workloads.find((x) => x.namespace === namespace && x.kind.toLowerCase() === kind && x.name === workload) ?? null;
  const pods = w && kube ? kube.pods.filter((p) => p.namespace === namespace && p.owner_kind === w.kind && p.owner_name === w.name) : [];
  const caps = e?.capabilities ?? [];
  const active = e?.state === 'active';
  const scope = `Cluster ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`;
  const owner = w?.instance && Array.isArray(applications.data) ? applications.data.find((i) => i.id === w.instance) : undefined;
  const managed = <p className="dr-alert dr-alert-warn">{owner && e ? <>Managed by application {displayName(owner.project)}. <Link to={envPath(org, e.environment_id)}>Edit it there.</Link></>
    : <>Managed by a KyYard application. <Link to={endpointPath(org, endpoint)}>Edit it there.</Link></>}</p>;
  const podProps = { base, org, endpoint, active, role, capabilities: caps, scope, onStatus: setStatus, onRefresh: inventory.reload };
  return <div className="ky-page ky-container-page">
    <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link><span>/</span><Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link></nav>
    <div className="ky-page-heading">
      <h1 style={{ fontSize: 24 }}><Layers size={24} style={{ color: 'var(--accent)' }} /><span>{namespace}/{workload}</span>{w && <span className="badge badge-secondary">{w.kind}</span>}</h1>
      {w && e && <WorkloadControls base={base} org={org} endpoint={endpoint} workload={w} active={active} role={role} capabilities={caps} scope={scope} onStatus={setStatus} onRefresh={inventory.reload} />}
    </div>
    {status && <p role="status">{status}</p>}
    <StateNotice state={details.state} onRetry={details.reload} />
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {e && e.runtime !== 'kubernetes' && <EmptyNotice>This endpoint is not a Kubernetes cluster.</EmptyNotice>}
    {inventory.state === 'ready' && !w && <EmptyNotice>This workload is no longer reported by <Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link>. It may have been deleted.</EmptyNotice>}
    {command && <section className="panel" aria-label="Last change">{command.outcome ? <WorkloadResult command={command} />
      : pollError ? <p role="alert" className="dr-alert dr-alert-error">Could not read the command result. Check the workload's Activity tab before trying again.</p>
      : <p role="status">Apply sent; waiting for the cluster agent. Do not retry while its outcome is unknown.</p>}</section>}
    {w && <>
      <nav aria-label="Workload sections" className="ky-resource-tabs">
        {tabs.map((t) => <button type="button" key={t} aria-pressed={tab === t} onClick={() => navigate(workloadPath(org, endpoint, namespace, kind, workload, t === 'overview' ? undefined : t))}>{t[0].toUpperCase() + t.slice(1)}</button>)}
      </nav>
      {tab === 'overview' && <Overview workload={w} pods={pods} controls={(p) => <PodControls {...podProps} pod={p} />} />}
      {tab === 'configuration' && (organizations.state === 'loading' || details.state === 'loading' ? <p role="status">Loading…</p> : !e ? null
        : !canConfigure(role) ? <Summary workload={w}><p>Only an organization administrator can edit this workload.</p></Summary>
        : w.application ? <Summary workload={w}>{managed}</Summary>
        : !caps.includes('kubernetes.workloads') ? <Summary workload={w}><EmptyNotice>{UPGRADE_CLUSTER}</EmptyNotice></Summary>
        : <EditWorkload key={`${namespace}/${kind}/${workload}/${settledId}`} base={base} target={{ namespace, kind, name: workload }} pending={!!command && !command.outcome} conflict={conflict} onSent={(cmd) => { setSent(cmd); if (cmd.outcome) inventory.reload(); }} managed={<Summary workload={w}>{managed}</Summary>} />)}
      {tab === 'logs' && <section className="panel" aria-label="Logs"><PodPicker pods={pods} initial={podParam}>{(pod) => <ContainerPicker pod={pod}>{(c) => <ContainerLogs key={`${pod.name}/${c}`} url={`${base}/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}/logs`} name={`${pod.namespace}/${pod.name}/${c}`} query={{ container: c }} />}</ContainerPicker>}</PodPicker></section>}
      {tab === 'terminal' && exec && <section className="panel" aria-label="Terminal">{!caps.includes('pod.exec') ? <EmptyNotice>{UPGRADE_CLUSTER}</EmptyNotice>
        : <PodPicker pods={pods} initial={podParam}>{(pod) => pod.phase === 'Running' && active
          ? <Suspense fallback={<p role="status">Loading terminal…</p>}><PodTerminal key={`${pod.name}/${pod.uid ?? ''}`} base={base} pod={pod} scope={scope} /></Suspense>
          : <EmptyNotice>The terminal needs a running pod on an active cluster.</EmptyNotice>}</PodPicker>}</section>}
      {tab === 'activity' && <Activity base={base} reference={`${namespace}/${kind}/${workload}`} cluster={endpointPath(org, endpoint)} />}
    </>}
  </div>;
};

function Overview({ workload: w, pods, controls }: { workload: Workload; pods: Pod[]; controls: (p: Pod) => ReactNode }) {
  const now = useNow();
  return <>
    <section className="panel" aria-label="Overview">
      <dl className="ky-facts">
        <dt>Kind</dt><dd>{w.kind}</dd>
        <dt>Replicas</dt><dd>{w.desired} desired · {w.ready} ready · {w.updated} updated</dd>
        {w.kind === 'Deployment' && <><dt>Rollout</dt><dd>{w.paused ? 'Paused' : 'Not paused'}</dd></>}
        <dt>Images</dt><dd>{w.images.length ? <ul className="ky-list">{w.images.map((i) => <li key={i} style={{ overflowWrap: 'anywhere' }}>{displayName(i)}</li>)}</ul> : '—'}</dd>
      </dl>
    </section>
    <ResourceTable title="Pods" rows={pods} rowKey={(p) => p.name} empty="No pods reported for this workload." head={['Pod', 'Phase', 'Uptime', 'Node', 'Restarts', 'Containers', 'Actions']} render={(p) => [
      <strong>{`${p.namespace}/${p.name}`}</strong>, displayName(p.phase), uptime(p.started_at, now) || '—', displayName(p.node) || '—',
      p.containers.reduce((sum, c) => sum + c.restart_count, 0),
      <ul className="ky-list">{p.containers.map((c) => <li key={c.name}>{displayName(c.name)} · {displayName(c.state)}{c.reason && ` ${displayName(c.reason)}`}</li>)}</ul>,
      controls(p),
    ]} />
  </>;
}

// Summary is the inventory's view of a workload, for whoever may not or cannot read its configuration.
function Summary({ workload: w, children }: { workload: Workload; children: ReactNode }) {
  return <section className="panel" aria-label="Configuration">
    {children}
    <dl className="ky-facts">
      <dt>Kind</dt><dd>{w.kind}</dd>
      <dt>Replicas</dt><dd>{w.desired}</dd>
      <dt>Images</dt><dd>{w.images.map(displayName).join(', ') || '—'}</dd>
    </dl>
  </section>;
}

// PodPicker selects one of the workload's pods, starting at the ?pod= link's when it is one of them.
function PodPicker({ pods, initial, children }: { pods: Pod[]; initial: string; children: (pod: Pod) => ReactNode }) {
  const [chosen, setChosen] = useState(initial);
  const pod = pods.find((p) => p.name === chosen) ?? pods[0];
  if (!pod) return <EmptyNotice>No pods reported for this workload.</EmptyNotice>;
  return <>
    <div className="ky-toolbar"><label>Pod <select value={pod.name} onChange={(e) => setChosen(e.target.value)}>{pods.map((p) => <option key={p.name} value={p.name}>{p.name} ({p.phase})</option>)}</select></label></div>
    {children(pod)}
  </>;
}

function ContainerPicker({ pod, children }: { pod: Pod; children: (container: string) => ReactNode }) {
  const [chosen, setChosen] = useState('');
  const c = pod.containers.find((x) => x.name === chosen) ?? pod.containers[0];
  if (!c) return <EmptyNotice>This pod reports no containers yet.</EmptyNotice>;
  return <>
    <div className="ky-toolbar"><label>Container <select value={c.name} onChange={(e) => setChosen(e.target.value)}>{pod.containers.map((x) => <option key={x.name} value={x.name}>{x.name}</option>)}</select></label></div>
    {children(c.name)}
  </>;
}

function Activity({ base, reference, cluster }: { base: string; reference: string; cluster: string }) {
  const commands = useTenantResource<Command[]>(`${base}/commands?reference=${encodeURIComponent(reference)}&limit=50`);
  return <section className="panel" aria-label="Activity">
    <div className="panel-header"><h2>Recent activity</h2><button className="btn-secondary" onClick={commands.reload}>Refresh</button></div>
    <p>Pod commands appear under the cluster's Activity tab on <Link to={cluster}>the cluster page</Link>.</p>
    <StateNotice state={commands.state} onRetry={commands.reload} />
    {commands.state === 'ready' && Array.isArray(commands.data) && (commands.data.length ? <ul className="ky-list">{commands.data.map((k) => <li key={k.id}>{`${new Date(k.created_at).toLocaleString()} · ${commandLine(k)}`}</li>)}</ul>
      : <EmptyNotice>No commands have been sent to this workload.</EmptyNotice>)}
  </section>;
}
