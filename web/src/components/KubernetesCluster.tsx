import { useEffect, useRef, useState } from 'react';
import { ContainerLogs } from './ContainerControls';
import { displayName } from './Endpoints';
import { ResourceTable } from './ResourceTable';
import { ManifestRegeneration } from './KubernetesManifest';
import type { ApplicationInstance } from './ApplicationAdoption';
import type { Endpoint, KubernetesInventory, Pod, PodContainer } from '../tenant';

const healthBadge: Record<string, string> = { healthy: 'badge-success', degraded: 'badge-danger' };
const stateBadge: Record<string, string> = { running: 'badge-success', terminated: 'badge-danger' };

// A Kubernetes endpoint shows health, nodes, workloads, pods with their logs, services, claims
// and the applications mapped to it. No Docker control is rendered, and none would be accepted.
// instances is null while the applications cannot be read.
export function KubernetesCluster({ org, base, endpoint, inventory, instances, admin, onChanged }: { org: string; base: string; endpoint: Endpoint; inventory: KubernetesInventory; instances: ApplicationInstance[] | null; admin: boolean; onChanged: () => void }) {
  const [namespace, setNamespace] = useState('');
  const [logs, setLogs] = useState<{ pod: Pod; container: PodContainer } | null>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { if (logs) dialog.current?.showModal(); }, [logs]);
  const health = endpoint.cluster_health ?? 'unknown';
  const ready = inventory.nodes.filter((n) => n.ready).length;
  // A namespace that left the cluster must not keep hiding everything.
  const selected = inventory.namespaces.includes(namespace) ? namespace : '';
  const scoped = <T extends { namespace: string }>(rows: T[]) => rows.filter((r) => selected === '' || r.namespace === selected);
  const qualified = (r: { namespace: string; name: string }) => `${displayName(r.namespace)}/${displayName(r.name)}`;
  const active = endpoint.state === 'active';
  return <>
    <section className="panel" aria-label="Cluster health">
      <h2 style={{ fontSize: 16 }}>Cluster health <span className={`badge ${healthBadge[health] ?? 'badge-secondary'}`}>{health}</span></h2>
      <p>{ready} of {inventory.nodes.length} nodes ready.</p>
    </section>
    <ResourceTable title="Nodes" rows={inventory.nodes} empty="No nodes reported." head={['Node', 'Ready', 'Roles', 'Version', 'Platform']} render={(n) => [
      <>{displayName(n.name)}{n.unschedulable && <> <span className="badge badge-secondary">cordoned</span></>}</>,
      <span className={`badge ${n.ready ? 'badge-success' : 'badge-danger'}`}>{n.ready ? 'ready' : 'not ready'}</span>,
      n.roles.map(displayName).join(', ') || '—', displayName(n.kubelet_version), `${displayName(n.os)}/${displayName(n.arch)}`,
    ]} />
    <div className="ky-toolbar">
      <label>Namespace <select aria-label="Namespace" value={selected} onChange={(e) => setNamespace(e.target.value)}>
        <option value="">All namespaces</option>
        {inventory.namespaces.map((ns) => <option key={ns} value={ns}>{displayName(ns)}</option>)}
      </select></label>
    </div>
    <ResourceTable key={`workloads-${selected}`} title="Workloads" rows={scoped(inventory.workloads)} empty="No workloads." head={['Workload', 'Kind', 'Ready', 'Images']} render={(w) => [
      qualified(w), w.kind, `${w.ready}/${w.desired}${w.paused ? ' (paused)' : ''}`, w.images.map(displayName).join(', '),
    ]} />
    <ResourceTable key={`pods-${selected}`} title="Pods" rows={scoped(inventory.pods)} empty="No pods." head={['Pod', 'Phase', 'Node', 'Restarts', 'Containers']} render={(p) => [
      <div className="ky-resource-name"><strong>{qualified(p)}</strong>{p.owner_kind && <small>{displayName(p.owner_kind)} {displayName(p.owner_name)}</small>}</div>,
      displayName(p.phase), displayName(p.node) || '—', p.containers.reduce((sum, c) => sum + c.restart_count, 0),
      <ul className="ky-list">{p.containers.map((c) => <li key={c.name}>
        {displayName(c.name)} <span className={`badge ${stateBadge[c.state] ?? 'badge-secondary'}`} title={c.image}>{displayName(c.state)}</span>{c.reason && ` ${displayName(c.reason)}`}{' '}
        <button className="btn-secondary" disabled={!active} aria-label={`Logs for ${p.namespace}/${p.name}/${c.name}`} onClick={() => setLogs({ pod: p, container: c })}>Logs</button>
      </li>)}</ul>,
    ]} />
    <ResourceTable key={`services-${selected}`} title="Services" rows={scoped(inventory.services)} empty="No services." head={['Service', 'Type', 'Cluster IP', 'Ports']} render={(s) => [
      qualified(s), displayName(s.type), displayName(s.cluster_ip) || '—', s.ports.map(displayName).join(', ') || '—',
    ]} />
    <ResourceTable key={`claims-${selected}`} title="Volume claims" rows={scoped(inventory.claims)} empty="No volume claims." head={['Claim', 'Phase', 'Storage class', 'Capacity']} render={(c) => [
      qualified(c), displayName(c.phase), displayName(c.storage_class) || '—', displayName(c.capacity) || '—',
    ]} />
    <section className="panel" aria-label="Applications"><h2 style={{ fontSize: 16 }}>Applications</h2>
      <p>{(endpoint.deploy_namespaces ?? []).length ? `KyYard deploys only to ${(endpoint.deploy_namespaces ?? []).join(', ')}, as the applied manifest grants.` : 'The manifest grants no namespace, so nothing deploys to this cluster.'}</p>
      {instances === null ? <p>Applications could not be read.</p> : instances.length === 0 ? <p>No application is mapped to this cluster.</p> : <ul className="ky-list">{instances.map((i) => {
        const deployments = inventory.workloads.filter((w) => w.kind === 'Deployment' && w.instance === i.id);
        const ready = deployments.filter((w) => w.desired > 0 && w.ready === w.desired).length;
        return <li key={i.id}><strong>{displayName(i.project)}</strong> · {displayName(i.namespace ?? '')} · {deployments.length ? `${ready} of ${deployments.length} Deployments ready` : 'not deployed'}</li>;
      })}</ul>}
      {admin && <ManifestRegeneration org={org} endpoint={endpoint} onSaved={onChanged} />}
    </section>
    {logs && <dialog ref={dialog} className="modal-window ky-log-dialog" aria-label={`Logs for ${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} onCancel={(event) => { event.preventDefault(); setLogs(null); }} onClose={() => setLogs(null)}>
      <button className="btn-secondary" onClick={() => setLogs(null)}>Close logs</button>
      <ContainerLogs key={`${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} url={`${base}/pods/${encodeURIComponent(logs.pod.namespace)}/${encodeURIComponent(logs.pod.name)}/logs`} name={`${logs.pod.namespace}/${logs.pod.name}/${logs.container.name}`} query={{ container: logs.container.name }} />
    </dialog>}
  </>;
}
