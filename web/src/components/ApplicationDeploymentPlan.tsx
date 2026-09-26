import { Fragment, useEffect, useState } from 'react';
import { useTenantResource, type Validation } from '../tenant';
import { ValidationLine } from './ApplicationValidation';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';
import { knownBlockers, messages, serviceFindings, MountList, type Mount } from './ApplicationPreflight';
import { unsupportedNames } from './ApplicationInspection';
import type { ApplicationInstance } from './ApplicationAdoption';

type ClaimMount = { claim: string; mount_path: string; read_only?: boolean };
type Claim = { name: string; storage_class: string; size: string; access_mode: string };
type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[]; pull_reference?: string; pull_digest?: string; mounts?: Mount[]; dropped_mounts?: Mount[]; object?: { namespace: string; name: string }; claim_mounts?: ClaimMount[] };
type DeployStep = { service: string; step: string; outcome: string; code?: string; detail: string };
// A Kubernetes identity names a Deployment (kind, namespace, name, uid) in place of a container.
type DeployedService = { service: string; container_id: string; image_id: string; created_unix: number; kind?: string; namespace?: string; name?: string; uid?: string };
type RemovalTarget = { service: string; container_id: string; image_id: string; created_unix: number; name: string };
type Deployment = { id: string; instance_id: string; endpoint_id: string; endpoint_name?: string; kind?: string; applied_by?: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; detail: string; correlation_id?: string; applied_at?: string | null; deadline?: string | null; settled_at?: string | null; result: { code?: string; steps: DeployStep[]; services: DeployedService[] } | null; plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[]; namespace?: string; claims?: Claim[] }; validation?: Validation; migration_id?: string; retained_claims?: string[] };
type Mapping = { instance_id: string; version: number; preview: { revision: number; project: string } };
type Props = { base: string; instanceID: string; latestRevision: number; instance: ApplicationInstance; refreshKey?: number };

const APPLY_CODES: Record<string, string> = {
  deployment_in_progress: 'A deployment is already in progress for this instance.',
  adoption_changed: 'Adoption changed. Refresh applications before applying.',
  endpoint_offline: 'The host is not connected. Reconnect it before applying.',
  deployment_not_sent: 'The deployment was not sent. Refresh and try again.',
};
const LEGACY_OUTCOME = 'The agent did not classify this outcome; upgrade the agent.';
// Step codes (docs/agent-protocol.md, Outcome codes). A step's detail is its code's parameter and
// renders only in the shape its code allows; an unknown code renders as inert text.
export const STEP_CODES: Record<string, string> = {
  runtime_unreadable: "The daemon's default runtime could not be read.",
  container_missing: 'The container no longer exists.',
  identity_mismatch: 'The container is not the one this plan was decided about.',
  image_identity_mismatch: 'The host reported a different image identity than the plan pinned.',
  configuration_unreported: "The runtime did not report the container's full configuration.",
  unsupported: 'The container has configuration the definition cannot express',
  bind_missing: 'A bind mount in the plan is not on the container.',
  volume_mount_missing: 'A kept volume is not mounted on the container.',
  volume_not_owned: 'The volume is not owned by this project.',
  volume_missing: 'The volume does not exist.',
  volume_create_failed: 'The volume could not be created.',
  image_missing: "The container's image is no longer present.",
  pinned_image_missing: 'The pinned image is not present on the host.',
  configuration_drift: 'The container changed after the precondition.',
  name_reserved: 'A container already holds the name reserved for the previous one.',
  name_taken: 'Something else already holds that name',
  identity_unusable: 'The runtime returned an unusable container identity.',
  identity_unreadable: 'The container started but its identity could not be read',
  identity_unverified: 'The container started but its identity could not be verified',
  dependents: 'Something still depends on this container.',
  deadline: 'Not enough time was left before the deadline to continue safely.',
  pull_failed: 'The image pull failed.',
  pull_unauthorized: 'The registry refused the credential.',
  pull_not_found: 'The registry has no such image.',
  pull_digest_mismatch: 'The pulled image does not match the pinned digest.',
  cancelled: 'The run was cancelled before the runtime answered.',
  runtime_timeout: 'The runtime did not answer in time.',
  runtime_error: 'The runtime call failed.',
  runtime_status: 'The runtime refused',
  forbidden: "The agent's access in the namespace does not allow this; apply the cluster's regenerated manifest.",
  rollout_timeout: 'The Deployment did not become available; it stays as applied',
  conflict: 'The object kept changing under the agent',
  pod_security: 'The namespace does not enforce Pod Security baseline; label it pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.',
  admission_denied: "The cluster refused the object (quota or policy); check the namespace's quotas and admission policies",
  claim_immutable: 'The claim exists with another StorageClass, size or access mode, and KyYard never changes a claim; delete it deliberately or choose its current settings',
  legacy: LEGACY_OUTCOME,
};
// CLAIM_RETAINED is a removal's skipped volume step with detail retained.
export const CLAIM_RETAINED = 'The claim and its data were kept; delete the PersistentVolumeClaim with kubectl when you no longer need it.';
export const RESULT_CODES: Record<string, string> = {
  step_failed: 'A step did not succeed; the steps say which.',
  clock_skew: 'The host clock differs from the server by more than five minutes; nothing ran.',
  invalid_request: 'The agent refused the deployment request as invalid.',
  wrong_endpoint: 'The deployment was addressed to another endpoint.',
  busy: 'The agent was already applying a deployment.',
  restarted: 'The agent restarted after replacement began; inspect the host.',
  unreadable: 'The runtime returned a result the agent could not read.',
  legacy: LEGACY_OUTCOME,
};
// Sentences the server itself writes on a row it settles without a result (internal/store,
// internal/api); any other row detail is an older agent's and stays hidden.
const SERVER_DETAILS = new Set([
  'the connection ended before a result arrived',
  'no result arrived before the deadline',
  "the host's result did not match the plan; inspect the host",
  'the endpoint disconnected before the deployment was sent',
  'the endpoint disconnected before the removal was sent',
]);
function codeText(table: Record<string, string>, code: string): string {
  return Object.hasOwn(table, code) ? table[code] ?? '' : `unrecognised outcome \`${code}\``;
}
export function stepText(s: { step?: string; outcome?: string; code?: string; detail?: string }): string {
  const code = s.code ?? '';
  if (!code) return s.step === 'volume' && s.outcome === 'skipped' && s.detail === 'retained' ? CLAIM_RETAINED : '';
  const text = codeText(STEP_CODES, code);
  const detail = s.detail ?? '';
  switch (code) {
    case 'unsupported': {
      const names = detail.split(',').filter(c => Object.hasOwn(unsupportedNames, c)).map(c => unsupportedNames[c]);
      return names.length ? `${text}: ${names.join(', ')}.` : `${text}.`;
    }
    case 'identity_unreadable':
    case 'identity_unverified':
      return /^[0-9a-f]{64}$/.test(detail) ? `${text} (container ${detail}).` : `${text}.`;
    case 'runtime_status':
      return /^[1-5][0-9]{2}$/.test(detail) ? `${text} with status ${detail}.` : `${text}.`;
    case 'admission_denied':
      return OBJECT.test(detail) ? `${text}. Object: ${detail}.` : `${text}.`;
    case 'name_taken':
    case 'conflict':
    case 'claim_immutable':
      return OBJECT.test(detail) ? `${text}: ${detail}.` : `${text}.`;
    case 'pod_security':
      return Object.hasOwn(POD_SECURITY, detail) ? POD_SECURITY[detail] ?? text : text;
    case 'rollout_timeout': {
      const parts = detail.split(',');
      const reasons = parts.filter((r) => ROLLOUT.test(r)).map((r) => r.replace('=', ' '));
      const claims = [...new Set(parts.filter((r) => Object.hasOwn(CLAIM_PHASES, r)))].map((r) => CLAIM_PHASES[r]);
      const base = reasons.length ? `${text} (${reasons.join(', ')})` : text;
      return claims.length ? `${base}; ${claims.join('; ')}.` : `${base}.`;
    }
  }
  return text;
}
// A pod_security refusal by the namespace's enforce label: absent, privileged, or not a level.
const POD_SECURITY: Record<string, string> = {
  missing: 'The namespace has no Pod Security enforce label; label it pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.',
  privileged: 'The namespace enforces Pod Security privileged, which lets a pod run privileged; set pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.',
  invalid: "The namespace's Pod Security enforce label is not a level Kubernetes knows; set pod-security.kubernetes.io/enforce=baseline (or restricted) and apply again.",
};
// A rollout_timeout's claim reasons: a planned claim the cluster has not bound.
const CLAIM_PHASES: Record<string, string> = {
  'claim=Pending': 'a volume claim is still Pending: no StorageClass provisioned it',
  'claim=Lost': 'a volume claim is Lost: the volume behind it is gone',
};
// The closed detail shapes of the Kubernetes codes: Kind/name, and condition=Reason words.
const OBJECT = /^(Deployment|Service|ConfigMap|Secret|PersistentVolumeClaim)\/[a-z0-9][-a-z0-9.]{0,252}$/;
const ROLLOUT = /^(progressing|available|replicafailure|pod)=[A-Za-z]{1,64}$/;
function isDeployment(x: unknown): x is Deployment {
  if (!x || typeof x !== 'object') return false;
  const d = x as Record<string, unknown>;
  return typeof d.id === 'string' && typeof d.instance_id === 'string' && typeof d.state === 'string'
    && typeof d.expires_at === 'string' && typeof d.expired === 'boolean'
    && typeof d.plan === 'object' && d.plan !== null;
}
function preconditionExplanation(code: string): string {
  switch (code) {
    case 'configuration_drift': return 'The mapped container changed on the host while the deployment prepared; it and every later service were left untouched, and services before it were replaced. Review the host, then plan again.';
    case 'container_missing': return 'The mapped container no longer exists on the host; refresh the inventory and plan again.';
    case 'identity_mismatch': return 'The mapped container changed on the host; plan again.';
    case 'image_missing': return "The mapped container's image is no longer present on the host. Review it on the host before planning again.";
    case 'legacy': return '';
  }
  return 'A mapped container has configuration the definition does not describe. Review it on the host before planning again.';
}
export const CLUSTER_STOPPED = 'The agent stopped before changing the cluster; the step below says why.';
export const CLUSTER_FAILED = 'A step failed on the cluster; objects written before it stay as applied, and nothing was rolled back.';
const CLUSTER_REMOVE_FAILED = 'A step failed on the cluster; objects removed before it are gone and the rest remain.';
// The row's state decides first: an unknown or timed-out row may carry no steps, and a failed
// row without a result never reached the host (FailDeployment). Then the result and step codes.
function explanationFor(current: Deployment): string {
  if (current.state === 'unknown') return 'The host may or may not have acted. Inspect it before planning again.';
  if (current.state === 'timed_out') return 'The host did not answer in time.';
  if (current.state === 'failed' && current.result === null) return 'The deployment was not sent to the host.';
  if (current.result?.code === 'clock_skew') return 'The host clock differs from the server by more than five minutes; nothing ran. Correct the host clock, then plan again.';
  const failing = current.result?.steps.find(s => s.outcome !== 'succeeded' && s.outcome !== 'skipped');
  if (!failing) return '';
  if (current.plan.namespace) {
    if (failing.outcome === 'denied' && failing.step === 'precondition') return CLUSTER_STOPPED;
    if (failing.outcome === 'failed') return current.kind === 'remove' ? CLUSTER_REMOVE_FAILED : CLUSTER_FAILED;
  }
  if (failing.outcome === 'denied' && (failing.step === 'precondition' || failing.step === 'recheck')) return current.kind === 'remove'
    ? 'A container of this application is not the one recorded; refresh the inventory and, if it was recreated outside KyYard, release and adopt it again.'
    : preconditionExplanation(failing.code ?? '');
  if (failing.code === 'pinned_image_missing') return 'The pinned image is no longer present on the host.';
  if (failing.outcome === 'unknown') return 'The host may or may not have acted. Inspect it before planning again.';
  if (failing.outcome === 'timed_out') return 'The host did not answer in time.';
  if (failing.outcome === 'failed' && current.kind === 'remove') return 'A step failed on the host; containers removed before it are gone and the rest stay adopted.';
  if (failing.outcome === 'failed') return 'A step failed on the host; the previous container may remain renamed with a .kyyard-prev suffix.';
  return '';
}

export function ApplicationDeploymentPlan(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? 'Close deployment plan' : 'Deployment plan'}</button>
    {open && <PlanView key={`${props.instanceID}/${props.latestRevision}/${props.refreshKey ?? 0}`} {...props} />}
  </section>;
}
function isExpired(d: Deployment): boolean {
  return d.expired || Date.parse(d.expires_at) <= Date.now();
}
function Correlation({ id }: { id?: string }) {
  return id ? <p>Correlation ID <code>{id}</code>: search the audit log for it.</p> : null;
}
function ResultSection({ current }: { current: Deployment }) {
  const steps = usePagination(current.result?.steps ?? [], `${current.id}-steps`);
  const explanation = explanationFor(current);
  const detail = SERVER_DETAILS.has(current.detail) ? current.detail : '';
  const outcome = current.result?.code ? codeText(RESULT_CODES, current.result.code) : '';
  return <>
    <p>State: {current.state}{current.settled_at ? `, settled ${new Date(current.settled_at).toLocaleString()}` : ''}.</p>
    <Correlation id={current.correlation_id} />
    {detail && <p>{detail}</p>}
    {outcome && <p>{outcome}</p>}
    {explanation && <p role="alert">{explanation}</p>}
    {current.validation && <p><ValidationLine v={current.validation} kubernetes={Boolean(current.plan.namespace)} /></p>}
    {current.result && <>
      {steps.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Step</th><th>Outcome</th><th>Detail</th></tr></thead><tbody>{steps.rows.map((s, i) => <tr key={`${s.service}-${s.step}-${i}`}>
        <td data-label="Service">{s.service}</td>
        <td data-label="Step">{s.step}</td>
        <td data-label="Outcome">{s.outcome}</td>
        <td data-label="Detail">{stepText(s)}</td>
      </tr>)}</tbody></table>
      <KeptClaims d={current} />
      {current.result.services.length > 0 && <ul className="ky-list">{current.result.services.map(s => <li key={s.service} style={{ overflowWrap: 'anywhere' }}><strong>{s.service}</strong><br />{s.kind === 'Deployment' ? <><span>Deployment {s.namespace}/{s.name}</span><br /><span>{s.uid}</span></> : <><span>{s.container_id}</span><br /><span>{s.image_id}</span></>}</li>)}</ul>}
    </>}
  </>;
}
// KeptClaims lists what a cluster removal left on the cluster: the claims the server can name, and
// how many more the agent reported, with the command that lists them all.
function KeptClaims({ d }: { d: Deployment }) {
  if (d.kind !== 'remove' || !d.plan.namespace || !d.result) return null;
  const kept = d.result.steps.filter(s => s.step === 'volume' && s.outcome === 'skipped' && s.detail === 'retained').length;
  const named = d.retained_claims ?? [];
  if (kept === 0) return null;
  return <>
    <h4>Kept on the cluster</h4>
    {named.length > 0 && <ul className="ky-list">{named.map(c => <li key={c}>PersistentVolumeClaim {d.plan.namespace}/{c}</li>)}</ul>}
    {kept > named.length && <p>{kept - named.length} more kept claim(s) are labelled for this instance: <code>kubectl -n {d.plan.namespace} get pvc -l kyyard.busnes.app/instance={d.instance_id}</code></p>}
  </>;
}
function PlanDetails({ d }: { d: Deployment }) {
  const services = usePagination(d.plan.services ?? [], `${d.id}-services`);
  const containers = usePagination(d.plan.containers ?? [], `${d.id}-containers`);
  if (d.kind === 'remove' && d.plan.namespace) return <p>Deletes the objects labelled as this instance's in namespace {d.plan.namespace}. Its PersistentVolumeClaims and their data are kept.</p>;
  if (d.kind === 'remove') return <>
    {containers.controls}
    <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Name</th><th>Container</th></tr></thead><tbody>{containers.rows.map(c => <tr key={c.container_id}>
      <td data-label="Service">{c.service}</td>
      <td data-label="Name">{c.name}</td>
      <td data-label="Container">{c.container_id}</td>
    </tr>)}</tbody></table>
  </>;
  return <>
    {d.plan.volumes?.length ? <p>Volumes to ensure: {d.plan.volumes.join(', ')}</p> : null}
    {d.plan.claims?.length ? <p>Claims to create when missing (never changed or deleted): {d.plan.claims.map(c => `${c.name} (${c.storage_class || 'cluster default'}, ${c.size})`).join(', ')}</p> : null}
    {services.controls}
    <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Pinned image</th><th>Replaces container</th><th>Mounts</th><th>Secrets</th></tr></thead><tbody>{services.rows.map(s => <tr key={s.name}>
      <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
      <td data-label="Pinned image"><div className="ky-resource-name">{/^sha256:[0-9a-f]{64}$/.test(s.pull_digest ?? '') ? <span>pulls {s.pull_digest?.slice(7, 19)}</span> : <><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></>}</div></td>
      <td data-label="Replaces container">{s.object ? <div className="ky-resource-name"><span>Deployment {s.object.namespace}/{s.object.name}</span><small>updated in place</small></div> : <div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div>}</td>
      <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : s.claim_mounts?.length ? <MountList mounts={s.claim_mounts.map(c => ({ kind: 'volume', source: c.claim, target: c.mount_path, read_only: c.read_only }))} /> : 'None'}{s.dropped_mounts?.length ? <><p>Will be dropped by the recreate:</p><MountList mounts={s.dropped_mounts} /></> : null}</td>
      <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
    </tr>)}</tbody></table>
  </>;
}
const when = (t?: string | null) => t ? new Date(t).toLocaleString() : '—';
// ApplicationHistory is the read-only history of an application with no instance, such as a
// removed one: every row, not one instance's.
export function ApplicationHistory({ base }: { base: string }) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? 'Close deployment history' : 'Deployment history'}</button>
    {open && <HistoryView base={base} />}
  </section>;
}
function HistoryView({ base }: { base: string }) {
  const deployments = useTenantResource<Deployment[]>(`${base}/deployments`);
  return <>
    <StateNotice state={deployments.state} onRetry={deployments.reload} />
    {deployments.state === 'ready' && <History rows={deployments.data ?? []} />}
  </>;
}
function History({ rows }: { rows: Deployment[] }) {
  const page = usePagination(rows, 'history');
  const [shown, setShown] = useState('');
  return <>
    <h3>Deployment history</h3>
    {rows.length === 0 ? <p>No deployments recorded.</p> : <>
      {page.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Kind</th><th>Revision</th><th>State</th><th>Applied by</th><th>Applied</th><th>Settled</th><th>Steps</th></tr></thead><tbody>{page.rows.map(d => <Fragment key={d.id}>
        <tr>
          <td data-label="Kind">{d.kind === 'remove' ? 'Removal' : d.migration_id ? 'Apply (migration)' : 'Apply'}</td>
          <td data-label="Revision">{d.revision}</td>
          <td data-label="State">{d.state}</td>
          <td data-label="Applied by">{d.applied_by || '—'}</td>
          <td data-label="Applied">{when(d.applied_at)}</td>
          <td data-label="Settled">{when(d.settled_at)}</td>
          <td data-label="Steps"><button type="button" className="btn-secondary" onClick={() => setShown(shown === d.id ? '' : d.id)}>{shown === d.id ? 'Hide steps' : 'Show steps'}</button></td>
        </tr>
        {shown === d.id && <tr><td colSpan={7}>{d.kind === 'remove' && <PlanDetails d={d} />}<ResultSection current={d} /></td></tr>}
      </Fragment>)}</tbody></table>
    </>}
  </>;
}
function PlanView({ base, instanceID, latestRevision, instance }: Props) {
  const mapping = useTenantResource<Mapping>(`${base}/mapping`);
  const deployments = useTenantResource<Deployment[]>(`${base}/deployments`);
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [error, setError] = useState<string[]>([]);
  const [planned, setPlanned] = useState<Deployment | null>(null);
  const found = deployments.data?.find(d => d.instance_id === instanceID) ?? null;
  useEffect(() => { if (found) setPlanned(found); }, [found]);
  const current = planned ?? found;
  const rows = deployments.data?.filter(d => d.instance_id === instanceID) ?? [];
  const history = current ? [current, ...rows.filter(d => d.id !== current.id)] : rows;
  const [revision, setRevision] = useState(latestRevision);
  const reload = () => { mapping.reload(); deployments.reload(); };
  const mismatched = mapping.data !== null && mapping.data.instance_id !== instanceID;

  const [applyConfirm, setApplyConfirm] = useState('');
  const [applyBusy, setApplyBusy] = useState(false);
  const [applyBlocked, setApplyBlocked] = useState(false);
  const [applyError, setApplyError] = useState('');
  const [pollPaused, setPollPaused] = useState(false);
  useEffect(() => { setApplyConfirm(''); setApplyBusy(false); setApplyBlocked(false); setApplyError(''); }, [current?.id]);
  // Polls the row directly (never through deployments.reload(), which resets that resource to
  // 'loading'/null and would unmount this whole section every tick). A held row keeps the panel
  // mounted; only the explicit "Refresh" buttons touch the shared deployments resource.
  useEffect(() => {
    setPollPaused(false);
    if (!current || current.state !== 'applying') return;
    const controller = new AbortController();
    // Poll until three minutes past the row's deadline (the server sweeps it to unknown two
    // minutes past), or 15 minutes from the first tick when the row carries none.
    const deadline = current.deadline ? Date.parse(current.deadline) : NaN;
    let stopAt = Number.isNaN(deadline) ? 0 : deadline + 3 * 60_000;
    let failures = 0;
    const id = window.setInterval(() => {
      if (!stopAt) stopAt = Date.now() + 15 * 60_000;
      if (Date.now() > stopAt) { setPollPaused(true); window.clearInterval(id); return; }
      fetch(`${base}/deployments`, { signal: controller.signal, cache: 'no-store' })
        .then(async r => {
          if (controller.signal.aborted) return;
          if (!r.ok) throw new Error('poll status');
          const body: unknown = await r.json();
          if (controller.signal.aborted) return;
          const row = Array.isArray(body) ? body.filter(isDeployment).find(d => d.instance_id === instanceID) : undefined;
          if (!row) throw new Error('poll body');
          failures = 0;
          setPollPaused(false);
          setPlanned(row);
          if (row.state !== 'applying') window.clearInterval(id);
        })
        .catch(() => {
          if (controller.signal.aborted) return;
          failures += 1;
          if (failures >= 3) { setPollPaused(true); window.clearInterval(id); }
        });
    }, 5000);
    return () => { controller.abort(); window.clearInterval(id); };
  }, [base, instanceID, current?.id, current?.state, current?.deadline]);

  const plan = async () => {
    if (!mapping.data) return;
    setBusy(true); setError([]);
    try {
      const r = await secureFetch(`${base}/deployments`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: mapping.data.instance_id, mapping_version: mapping.data.version, revision, confirm }) });
      if (r.ok) {
        const body: unknown = await r.json().catch(() => null);
        if (body && typeof body === 'object' && 'instance_id' in body) setPlanned(body as Deployment);
        setConfirm('');
        return;
      }
      setBlocked(true);
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const lines = [...knownBlockers(payload, messages).map(b => messages[b]), ...serviceFindings(payload)];
        setError(lines.length ? lines : ['Ownership, mapping or the definition changed. Refresh applications and review before planning again.']);
        return;
      }
      setError([r.status === 403 ? 'You do not have permission to plan deployments.' : 'The plan was refused or its outcome is unknown. Refresh before trying again.']);
    } catch { setBlocked(true); setError(['The outcome is unknown. Refresh before trying again.']); }
    finally { setBusy(false); }
  };
  const apply = async () => {
    if (!current) return;
    setApplyBusy(true); setApplyError('');
    try {
      const r = await secureFetch(`${base}/deployments/${current.id}/apply`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ confirm: applyConfirm }) });
      if (r.status === 202) {
        const body: unknown = await r.json().catch(() => null);
        if (body && typeof body === 'object' && 'instance_id' in body) setPlanned(body as Deployment);
        setApplyConfirm('');
        return;
      }
      setApplyBlocked(true);
      if (r.status === 501) { setApplyError('Upgrade the host agent to enable deployments.'); return; }
      if (r.status === 403) { setApplyError('You do not have permission to apply deployments.'); return; }
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const code = payload && typeof payload === 'object' && 'code' in payload ? (payload as { code?: unknown }).code : undefined;
        const blockers = code === 'preflight_blocked' ? knownBlockers(payload, messages).map(b => messages[b]) : [];
        if (blockers.length) { setApplyError(blockers.join(' ')); return; }
        setApplyError((typeof code === 'string' && APPLY_CODES[code]) || 'The apply was refused or its outcome is unknown. Refresh before trying again.');
        return;
      }
      setApplyError('The apply was refused or its outcome is unknown. Refresh before trying again.');
    } catch { setApplyBlocked(true); setApplyError('The outcome is unknown. Refresh before trying again.'); }
    finally { setApplyBusy(false); }
  };
  return <>
    <p>A plan records the exact revision, mapping and image identities a deployment would use. Planning executes nothing: no containers change, no images are pulled and no secret values are read. Runtime configuration remains unverified.</p>
    <p>Current revision {instance.current_revision || 'none'} · previous {instance.previous_revision || 'none'}</p>
    <StateNotice state={deployments.state} onRetry={reload} />
    {deployments.state === 'ready' && <>
      {mismatched && <p role="alert">Adoption changed. Refresh applications before planning.</p>}
      {pollPaused && <p role="status">Status updates paused; refresh to continue.</p>}
      {!mismatched && (current ? <>
        {current.kind === 'remove'
          ? <p>Removal of project <bdi>{current.plan.project}</bdi> on {current.endpoint_name || current.endpoint_id}: these containers are stopped and removed.</p>
          : <p>Plan for revision {current.revision}, mapping version {current.mapping_version}, project <bdi>{current.plan.project}</bdi>. {isExpired(current) ? 'This plan has expired; plan again to continue.' : `Expires ${new Date(current.expires_at).toLocaleString()}.`}</p>}
        <PlanDetails d={current} />
        {current.state === 'planned' && <Correlation id={current.correlation_id} />}
      </> : <p>No plan for this instance.</p>)}
      {!mismatched && current && current.state === 'planned' && !isExpired(current) && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void apply(); }}>
        {current.plan.namespace
          ? <p>Applying updates the Deployments, Services, ConfigMaps and Secrets of {current.plan.project} in namespace {current.plan.namespace} to revision {current.revision} and waits for each rollout. Nothing rolls back on failure; a failed run leaves the objects as applied.</p>
          : <p>Applying replaces the mapped containers on {current.endpoint_id} with revision {current.revision} of {current.plan.project}. Nothing rolls back on failure; a failed run leaves the previous container renamed on the host.</p>}
        <label>Confirm apply project<input value={applyConfirm} onChange={e => setApplyConfirm(e.target.value)} disabled={applyBusy || applyBlocked} autoComplete="off" /></label>
        <button disabled={applyBusy || applyBlocked || applyConfirm !== current.plan.project}>Apply deployment</button>
        {applyError && <p role="alert">{applyError}</p>}
      </form>}
      {!mismatched && current && current.state !== 'planned' && <ResultSection current={current} />}
      {!mismatched && <History rows={history} />}
    </>}
    {mapping.state === 'ready' && !mismatched && mapping.data && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void plan(); }}>
      <label>Revision to plan<select value={revision} onChange={e => setRevision(Number(e.target.value))} disabled={busy || blocked}>{Array.from({ length: latestRevision }, (_, i) => latestRevision - i).map(n => <option key={n} value={n}>Revision {n}{n === latestRevision ? ' · latest' : ''}</option>)}</select></label>
      {revision < latestRevision && <p role="note">Revision {revision} uses its own saved environment values. Data written since is not reversed. The service mapping was reviewed against revision {latestRevision}.</p>}
      <p>Planning replaces any earlier plan for this instance. Type the project name <bdi>{mapping.data.preview.project}</bdi> to confirm planning revision {revision}. Nothing runs.</p>
      <label>Confirm plan project<input value={confirm} onChange={e => setConfirm(e.target.value)} disabled={busy || blocked} autoComplete="off" /></label>
      <button disabled={busy || blocked || confirm !== mapping.data.preview.project}>Plan deployment</button>
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => { setBlocked(false); setError([]); setPlanned(null); reload(); }}>Refresh plan</button>
      {error.length > 0 && <ul role="alert">{error.map(e => <li key={e}>{e}</li>)}</ul>}
    </form>}
    {mapping.state !== 'ready' && <StateNotice state={mapping.state} onRetry={mapping.reload} />}
  </>;
}
