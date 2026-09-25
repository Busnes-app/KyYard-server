import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { ManifestRegeneration } from './KubernetesManifest';
import { displayName } from './Endpoints';
import type { ApplicationInstance } from './ApplicationAdoption';

const MAPPING_REFUSALS: Record<string, string> = {
  namespace_unknown: "The cluster's manifest does not grant that namespace. Regenerate the manifest, apply it, then map again.",
  application_adopted: 'The application is mapped to another endpoint, or was deployed in its current namespace. Remove it there before moving it.',
  deployment_in_progress: 'A deployment is being applied; wait for its result.',
  runtime_unsupported: 'That endpoint is not a Kubernetes cluster.',
};

// KubernetesMapping maps the application to a namespace of a Kubernetes cluster: the namespace
// is chosen from the ones the cluster's manifest grants, and an administrator can regenerate the
// manifest beside it. Nothing is deployed until a plan is applied.
export function KubernetesMapping({ base, org, env, instance, admin, onChanged }: { base: string; org: string; env: string; instance?: ApplicationInstance; admin: boolean; onChanged: (status?: string) => void }) {
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/endpoints?limit=200`);
  const clusters = (Array.isArray(endpoints.data) ? endpoints.data : []).filter((e) => e.runtime === 'kubernetes');
  const [chosen, setChosen] = useState(instance?.endpoint_id ?? '');
  const [namespace, setNamespace] = useState(instance?.namespace ?? '');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const cluster = clusters.find((c) => c.id === chosen);
  if (endpoints.state === 'ready' && clusters.length === 0) return null;
  const save = async () => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(`${base}/mapping`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ endpoint_id: chosen, namespace }) });
      if (r.ok) { onChanged(`Mapped to namespace ${namespace}. Nothing was deployed.`); return; }
      const payload = await r.json().catch(() => ({})) as { code?: unknown };
      const code = typeof payload.code === 'string' ? payload.code : '';
      setMessage(r.status === 403 ? 'Only an administrator can map applications.' : Object.hasOwn(MAPPING_REFUSALS, code) ? MAPPING_REFUSALS[code] : r.status === 409 ? 'Another application on this cluster has the same name, or the mapping changed. Refresh applications.' : 'The mapping was refused. Refresh applications and try again.');
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  return <section className="dr-stack" aria-label="Kubernetes mapping">
    <h3>{instance ? `Kubernetes namespace ${instance.namespace}` : 'Deploy to a Kubernetes cluster'}</h3>
    <p>Each service becomes a Deployment, a Service, a ConfigMap and a Secret in the namespace, labelled as this application's. A service with a volume, a host address or a restart policy other than always stops at the plan with the reason.</p>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    {endpoints.state === 'ready' && <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <label>Cluster<select value={chosen} disabled={busy || Boolean(instance)} onChange={(e) => { setChosen(e.target.value); setNamespace(''); }}>
        <option value="">Choose a cluster</option>
        {clusters.map((c) => <option key={c.id} value={c.id}>{displayName(c.name)} · {c.state}</option>)}
      </select></label>
      {cluster && ((cluster.deploy_namespaces ?? []).length > 0
        ? <label>Namespace<select value={namespace} disabled={busy} onChange={(e) => setNamespace(e.target.value)}>
          <option value="">Choose a namespace</option>
          {(cluster.deploy_namespaces ?? []).map((ns) => <option key={ns} value={ns}>{ns}</option>)}
        </select></label>
        : <p>This cluster's manifest grants no namespace yet{admin ? '. Regenerate it below and apply it.' : '; ask an administrator to regenerate it.'}</p>)}
      <button disabled={busy || !cluster || !namespace || namespace === instance?.namespace}>Save namespace mapping</button>
      {message && <p role="alert">{message}</p>}
    </form>}
    {cluster && admin && <ManifestRegeneration key={cluster.id} org={org} endpoint={cluster} onSaved={endpoints.reload} />}
  </section>;
}

// KUBERNETES_UNVALIDATED is the one sentence the application page gives a Kubernetes instance.
export const KUBERNETES_UNVALIDATED = 'KyYard does not read health from a Kubernetes Deployment yet: the rollout wait is the apply\'s health check, and nothing is rolled back automatically. To go back, plan and apply the earlier revision.';

// KubernetesWorkloads lists the instance's Deployments as the cluster last reported them.
export function KubernetesWorkloads({ org, instance }: { org: string; instance: ApplicationInstance }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(instance.endpoint_id)}/inventory`);
  const rows = (inventory.data?.snapshot.kubernetes?.workloads ?? []).filter((w) => w.kind === 'Deployment' && w.instance === instance.id);
  return <section className="dr-stack" aria-label="Kubernetes Deployments">
    <h3>Deployments in {instance.namespace} on {instance.endpoint_name}</h3>
    <p>{KUBERNETES_UNVALIDATED}</p>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && (rows.length === 0 ? <p>No Deployment of this application is reported yet.</p> : <table className="ky-table ky-responsive-table"><thead><tr><th>Deployment</th><th>Ready</th><th>Images</th></tr></thead><tbody>{rows.map((w) => <tr key={w.name}>
      <td data-label="Deployment">{displayName(w.name)}</td>
      <td data-label="Ready">{w.ready}/{w.desired}{w.paused ? ' (paused)' : ''}</td>
      <td data-label="Images" style={{ overflowWrap: 'anywhere' }}>{w.images.map(displayName).join(', ')}</td>
    </tr>)}</tbody></table>)}
  </section>;
}
