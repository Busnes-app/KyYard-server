import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { displayName } from './Endpoints';
import { unsupportedNames } from './ApplicationInspection';
import type { ApplicationInstance } from './ApplicationAdoption';

// The analyzer's codes (internal/migration), checked against web/src/migration-codes.json.
export const MIGRATION_CODES: Record<string, string> = {
  volume_named: 'A named volume becomes a PersistentVolumeClaim once you choose its StorageClass and size.',
  volume_named_shared: 'Two services mount this volume; a ReadWriteOnce claim serves one pod, so it cannot move as it is.',
  volume_bind: 'A host path cannot follow the service to a cluster.',
  volume_external: 'An external volume moves as a new claim (KyYard adopts no existing claim); choose its StorageClass and size.',
  volume_unverified: "The running container does not mount this volume, so its data cannot be verified as the application's; adopt the container that mounts it or remove the volume.",
  storage_supported: 'No volumes.',
  network_host: 'Host networking has no equivalent on the cluster.',
  networks_multiple: 'On the cluster, services reach each other only as <project>-<service> on published ports.',
  network_references: 'On the cluster the other services reach this one only by its destination name, on its published ports; update their references to it, then acknowledge.',
  networking_supported: 'A single service: no other service addresses it by name.',
  port_published: 'Published as a ClusterIP Service; exposing it outside the cluster is your Ingress.',
  port_host_ip: 'A port bound to one host address has no Service equivalent; drop the address.',
  port_unpublished: 'Publishes no port, so it gets no Service and no other service on the cluster can reach it.',
  secrets_supported: "Environment values move into a Secret, copied from the source's encrypted values.",
  healthcheck_dropped: "The container's healthcheck is not rendered as a probe; acknowledge the drop, then add a probe after cutover or rely on the rollout wait.",
  probes_supported: 'No healthcheck to carry over.',
  resource_limits_dropped: 'Resource limits are not rendered; acknowledge the drop, then set them on the Deployment after cutover or run without them.',
  resources_supported: 'No resource limits to carry over.',
  scheduling_blocked: 'Shares a host namespace or uses a runtime setting a Deployment does not express.',
  scheduling_supported: 'Nothing host-specific to schedule.',
  flag_blocked: "Runs with a host privilege or setting that Pod Security baseline refuses or KyYard does not carry.",
  restart_policy: 'A Deployment always restarts its pod; set the restart policy to always or unless-stopped first.',
  read_only_rootfs: 'The root filesystem is read-only on the host; the destination runs it writable until you set readOnlyRootFilesystem. Acknowledge the drop.',
  flags_supported: 'No privileged flags.',
  inspection_unavailable: 'The host answered no live inspection, so this is unknown; analyze again when the host is online.',
};
export const CHECKLIST_STEPS: Record<string, string> = {
  grant_namespace: "Label the namespace to enforce Pod Security baseline. The command refuses to change an existing label, so a namespace already at restricted stays there, and restricted refuses the destination's pods, which carry no security context. Then regenerate the cluster's manifest and apply it, and set the agent Deployment's image to the current pinned digest: migrations need the claim and StorageClass rules and an agent that applies claims.",
  create_destination: 'Create the destination below, then plan and apply it from its own entry in Applications.',
  update_references: "Change every reference one service makes to another by its Compose name (environment values, configuration) to the destination name, then save the destination's definition and apply it:",
  copy_volume: "Stop writes to the source. Set HELPER_IMAGE to a digest-pinned image that has sh and tar; both sides use it. Each Deployment is scaled to 0; a helper pod mounts each claim, the destination's first-start data is removed from it, and the source volume is copied in; then the helper is deleted and the Deployment scaled back to 1.",
  validate_destination: 'Check the destination works, then confirm validation below.',
  switch_traffic: 'Point your DNS or Ingress at the destination.',
  confirm_cutover: "Confirm cutover below, then remove the source with KyYard's removal. KyYard never stops or removes it for you.",
};
// INSPECTION_AGENT replaces inspection_unavailable's sentence when its detail is agent: the host
// answered, but its agent reports no health.
export const INSPECTION_AGENT = "The host's agent does not report container health, so this is unknown; upgrade the Docker agent, then analyze again.";
export const ASSUMPTIONS: Record<string, string> = {
  volume_size_unknown: 'Docker reports no volume size; size each claim yourself.',
};
export const MIGRATION_ERRORS: Record<string, string> = {
  namespace_unknown: "The cluster's manifest does not grant that namespace.",
  runtime_unsupported: 'The source must be adopted on a Docker host, and the destination must be a Kubernetes cluster.',
  mapping_required: 'Adopt and map the application on a Docker host first.',
  adoption_changed: "The source host's inventory is stale or changed; refresh it and try again.",
  migration_open: 'This application already has an open migration.',
  storage_class_unknown: "The cluster does not report that StorageClass. If the list is empty, apply the regenerated manifest and set the agent Deployment's image to the current pinned digest.",
  size_invalid: 'A size is a whole number of Mi, Gi or Ti, from 1Mi to 16Ti.',
  volume_unknown: 'The source mounts no named volume by that name.',
  application_name_taken: 'Applications already hold the destination name and every suffix up to (9). Discard one of them first.',
  destination_name_too_long: "The destination's name, the source's name plus \" on \" and the cluster's, would be longer than 255 characters. Rename the cluster.",
  inventory_stale: "The cluster's inventory is stale. Wait for the agent's next report, then try again.",
  migration_not_ready: 'Resolve every blocked finding and make every choice first.',
  migration_state: 'The migration moved on; refresh it.',
  migration_stale: 'The source definition changed since the analysis; analyze again.',
};
// ACKNOWLEDGEMENTS are the findings answered by acknowledging them (store.MigrationAcknowledgeable).
export const ACKNOWLEDGEMENTS: Record<string, string> = {
  network_references: 'I will update every reference to the destination names in the checklist.',
  port_unpublished: 'I accept that a service publishing no port is unreachable from the others.',
  healthcheck_dropped: 'I accept that the destination runs without the healthcheck.',
  resource_limits_dropped: 'I accept that the destination runs without the resource limits.',
  read_only_rootfs: 'I accept that the destination runs with a writable root filesystem.',
};
const CLASSES: Record<string, string> = { supported: 'Supported', operator_choice_required: 'Choice required', blocked: 'Blocked' };
const STATUSES: Record<string, string> = { analyzed: 'Analyzed', destination_created: 'Destination created', validated: 'Validated', cutover_confirmed: 'Cutover confirmed', abandoned: 'Abandoned' };
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

type Finding = { axis: string; class: string; code: string; detail?: string };
type Report = { version: number; ready: boolean; services: { name: string; class: string; findings: Finding[] }[]; checklist: { code: string; commands?: string[] }[]; assumptions: string[] };
type Choice = { storage_class: string; size: string; access_mode: string };
export type Migration = { id: string; application_id: string; application_name: string; destination_application_id?: string; destination_application_name?: string; destination_endpoint_id: string; namespace: string; status: string; ready: boolean; report: Report; choices: { volumes: Record<string, Choice>; acknowledged?: string[] }; role: 'source' | 'destination' };

// isMigration holds a response to the shape this card renders; anything else renders nothing.
function isMigration(x: unknown): x is Migration {
  if (!x || typeof x !== 'object') return false;
  const m = x as Record<string, unknown>;
  const r = m.report as Record<string, unknown> | null | undefined;
  return (m.role === 'source' || m.role === 'destination') && typeof m.status === 'string' && !!r && typeof r === 'object' && Array.isArray(r.services) && Array.isArray(r.checklist) && Array.isArray(r.assumptions);
}

// findingText is a finding's sentence with its parameter: a code from UnsupportedCodes by its
// name, anything else as inert text; an unknown code renders nothing.
export function findingText(f: Finding): string {
  if (f.code === 'inspection_unavailable' && f.detail === 'agent') return INSPECTION_AGENT;
  const text = fixed(MIGRATION_CODES, f.code);
  if (!text || !f.detail) return text;
  if (Object.hasOwn(unsupportedNames, f.detail)) return `${text} (${unsupportedNames[f.detail]})`;
  return `${text} (${f.detail})`;
}

// ApplicationMigration is the Migration card: on a Docker-adopted source, an administrator
// analyzes it against a cluster, makes the storage choices, creates the destination and confirms
// the operator's steps; a destination shows where it came from. KyYard never stops the source.
export function ApplicationMigration({ base, org, env, instance, admin, onOpen }: { base: string; org: string; env: string; instance: ApplicationInstance; admin: boolean; onOpen: (application: string) => void }) {
  const migration = useTenantResource<Migration>(`${base}/migration`);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const m = migration.data;
  const write = async (method: string, path: string, body?: unknown) => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(`${base}/migration${path}`, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
      if (r.ok) {
        const payload = await r.json().catch(() => ({})) as { destination_kept?: unknown };
        if (method === 'DELETE') setMessage(payload.destination_kept === true ? 'Migration abandoned. The destination application stays; remove it yourself if you no longer want it.' : 'Migration abandoned.');
        migration.reload();
        return;
      }
      const payload = await r.json().catch(() => ({})) as { code?: unknown };
      const code = typeof payload.code === 'string' ? payload.code : '';
      setMessage(r.status === 403 ? 'Only an organization administrator can migrate applications.' : fixed(MIGRATION_ERRORS, code) || 'The request was refused. Refresh the migration and try again.');
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  if (migration.state === 'notfound') return !instance.namespace && admin ? <MigrationStart org={org} env={env} admin={admin} busy={busy} message={message} onStart={(body) => void write('POST', '', body)} /> : null;
  if (migration.state !== 'ready') return <StateNotice state={migration.state} onRetry={migration.reload} />;
  if (!isMigration(m)) return null;
  if (m.role === 'destination') return <section className="dr-stack" aria-label="Migration">
    <p>Migration destination of {m.application_name} · {fixed(STATUSES, m.status)}</p>
    <button type="button" className="btn-secondary" onClick={() => onOpen(m.application_id)}>Open {m.application_name}</button>
  </section>;
  const report = m.report;
  return <section className="dr-stack" aria-label="Migration" style={{ overflowWrap: 'anywhere' }}>
    <h3>Migration to namespace {m.namespace} · {fixed(STATUSES, m.status)}</h3>
    <p>{report.ready ? 'Ready: every service can run on the cluster as analyzed.' : 'Not ready: resolve the blocked findings and make the choices below, then analyze again.'}</p>
    <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Axis</th><th>Class</th><th>Finding</th></tr></thead><tbody>{report.services.flatMap((s) => s.findings.map((f, i) => <tr key={`${s.name}/${i}`}>
      <td data-label="Service">{i === 0 ? <strong>{s.name}</strong> : ''}</td>
      <td data-label="Axis">{f.axis}</td>
      <td data-label="Class"><span className="badge">{fixed(CLASSES, f.class)}</span></td>
      <td data-label="Finding">{findingText(f)}</td>
    </tr>))}</tbody></table>
    {report.assumptions.map((a) => <p key={a}>{fixed(ASSUMPTIONS, a)}</p>)}
    {admin && m.status === 'analyzed' && <MigrationChoices key={JSON.stringify(m.choices)} org={org} migration={m} busy={busy} onSave={(volumes, acknowledged) => void write('PUT', '/choices', { volumes, acknowledged })} />}
    {admin && m.status === 'analyzed' && <div className="ky-inline-form">
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => void write('POST', '/analyze')}>Analyze again</button>
      <button type="button" disabled={busy || !m.ready} onClick={() => void write('POST', '/destination')}>Create destination</button>
    </div>}
    {m.destination_application_id && <div className="ky-inline-form"><p>Destination application {m.destination_application_name}: plan and apply it from its own entry.</p><button type="button" className="btn-secondary" onClick={() => onOpen(m.destination_application_id ?? '')}>Open {m.destination_application_name}</button></div>}
    <h4>Checklist</h4>
    <ol>{report.checklist.map((step) => <li key={step.code}>{fixed(CHECKLIST_STEPS, step.code)}{step.commands?.map((c) => <pre key={c}><code>{c}</code></pre>)}</li>)}</ol>
    {admin && m.status === 'destination_created' && <MigrationConfirm label="Confirm validation" busy={busy} onConfirm={(note) => void write('POST', '/validated', { note })} />}
    {admin && m.status === 'validated' && <MigrationConfirm label="Confirm cutover" busy={busy} onConfirm={(note) => void write('POST', '/cutover', { note })} />}
    {admin && m.status !== 'abandoned' && m.status !== 'cutover_confirmed' && <button type="button" className="btn-danger" disabled={busy} onClick={() => { if (window.confirm('Abandon this migration? The source keeps running and a created destination stays.')) void write('DELETE', ''); }}>Abandon migration</button>}
    {message && <p role="alert">{message}</p>}
  </section>;
}

function MigrationStart({ org, env, admin, busy, message, onStart }: { org: string; env: string; admin: boolean; busy: boolean; message: string; onStart: (body: { destination_endpoint_id: string; namespace: string }) => void }) {
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/endpoints?limit=200`);
  const clusters = (Array.isArray(endpoints.data) ? endpoints.data : []).filter((e) => e.runtime === 'kubernetes');
  const [cluster, setCluster] = useState('');
  const [namespace, setNamespace] = useState('');
  const chosen = clusters.find((c) => c.id === cluster);
  if (endpoints.state === 'ready' && clusters.length === 0) return null;
  return <section className="dr-stack" aria-label="Migration">
    <h3>Migrate to a Kubernetes cluster</h3>
    <p>Analysis reads the definition and a live inspection of each container. Nothing runs and the source keeps running.</p>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); onStart({ destination_endpoint_id: cluster, namespace }); }}>
      <label>Destination cluster<select value={cluster} disabled={busy} onChange={(e) => { setCluster(e.target.value); setNamespace(''); }}>
        <option value="">Choose a cluster</option>
        {clusters.map((c) => <option key={c.id} value={c.id}>{displayName(c.name)}</option>)}
      </select></label>
      {chosen && ((chosen.deploy_namespaces ?? []).length > 0
        ? <label>Destination namespace<select value={namespace} disabled={busy} onChange={(e) => setNamespace(e.target.value)}>
            <option value="">Choose a namespace</option>
            {(chosen.deploy_namespaces ?? []).map((ns) => <option key={ns} value={ns}>{ns}</option>)}
          </select></label>
        : <p>This cluster's manifest grants no namespace yet{admin ? '. Regenerate it below and apply it.' : '; ask an administrator to regenerate it.'}</p>)}
      <button disabled={busy || !cluster || !namespace}>Analyze</button>
      {message && <p role="alert">{message}</p>}
    </form>
  </section>;
}

function MigrationChoices({ org, migration, busy, onSave }: { org: string; migration: Migration; busy: boolean; onSave: (volumes: Record<string, Choice>, acknowledged: string[]) => void }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(migration.destination_endpoint_id)}/inventory`);
  const classes = inventory.data?.snapshot.kubernetes?.storage_classes ?? [];
  const fallback = classes.find((c) => c.default);
  const volumes = [...new Set(migration.report.services.flatMap((s) => s.findings.filter((f) => (f.code === 'volume_named' || f.code === 'volume_external') && f.detail).map((f) => f.detail ?? '')))];
  const [choices, setChoices] = useState<Record<string, Choice>>(() => Object.fromEntries(volumes.map((v) => [v, migration.choices.volumes[v] ?? { storage_class: '', size: '', access_mode: 'ReadWriteOnce' }])));
  const reported = Object.keys(ACKNOWLEDGEMENTS).filter((code) => migration.report.services.some((s) => s.findings.some((f) => f.code === code)));
  const [acknowledged, setAcknowledged] = useState<string[]>(() => (migration.choices.acknowledged ?? []).filter((code) => reported.includes(code)));
  if (volumes.length === 0 && reported.length === 0) return null;
  const set = (v: string, patch: Partial<Choice>) => setChoices({ ...choices, [v]: { ...choices[v], ...patch } });
  return <form className="dr-stack" aria-label="Choices" onSubmit={(e) => { e.preventDefault(); onSave(choices, acknowledged); }}>
    {reported.length > 0 && <fieldset>
      <legend>Acknowledgements</legend>
      {reported.map((code) => <label key={code}><input type="checkbox" checked={acknowledged.includes(code)} disabled={busy} onChange={(e) => setAcknowledged(e.target.checked ? [...acknowledged, code] : acknowledged.filter((c) => c !== code))} /> {ACKNOWLEDGEMENTS[code]}</label>)}
    </fieldset>}
    {volumes.length > 0 && <h4>Storage choices</h4>}
    {volumes.length > 0 && <StateNotice state={inventory.state} onRetry={inventory.reload} />}
    {volumes.map((v) => <fieldset key={v}>
      <legend>Volume {v}</legend>
      <label>StorageClass for {v}<select value={choices[v]?.storage_class ?? ''} disabled={busy} onChange={(e) => set(v, { storage_class: e.target.value })}>
        {fallback ? <option value="">Cluster default ({displayName(fallback.name)})</option> : <option value="">Choose a StorageClass</option>}
        {classes.map((c) => <option key={c.name} value={c.name}>{displayName(c.name)}</option>)}
      </select></label>
      <label>Size for {v}<input value={choices[v]?.size ?? ''} placeholder="10Gi" disabled={busy} onChange={(e) => set(v, { size: e.target.value })} /></label>
      <p>Access mode: ReadWriteOnce.</p>
    </fieldset>)}
    <button disabled={busy}>Save choices</button>
  </form>;
}

function MigrationConfirm({ label, busy, onConfirm }: { label: string; busy: boolean; onConfirm: (note: string) => void }) {
  const [note, setNote] = useState('');
  return <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); onConfirm(note); }}>
    <label>Note for {label.toLowerCase()}<input value={note} maxLength={500} disabled={busy} onChange={(e) => setNote(e.target.value)} /></label>
    <button disabled={busy || !note.trim()}>{label}</button>
  </form>;
}
