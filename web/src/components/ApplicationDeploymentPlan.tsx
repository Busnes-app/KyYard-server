import { useEffect, useState } from 'react';
import { useTenantResource } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';
import { messages } from './ApplicationPreflight';

type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[] };
type DeployStep = { service: string; step: string; outcome: string; detail: string };
type DeployedService = { service: string; container_id: string; image_id: string; created_unix: number };
type Deployment = { id: string; instance_id: string; endpoint_id: string; state: string; revision: number; mapping_version: number; created_at: string; expires_at: string; expired: boolean; detail: string; applied_at?: string | null; settled_at?: string | null; result: { steps: DeployStep[]; services: DeployedService[] } | null; plan: { project: string; services: PlannedService[] } };
type Mapping = { instance_id: string; version: number; preview: { revision: number; project: string } };
type Props = { base: string; instanceID: string };

const APPLY_CODES: Record<string, string> = {
  deployment_in_progress: 'A deployment is already in progress for this instance.',
  adoption_changed: 'Adoption changed. Refresh applications before applying.',
  endpoint_offline: 'The host is not connected. Reconnect it before applying.',
  deployment_not_sent: 'The deployment was not sent. Refresh and try again.',
};
// Fixed adapter detail prefixes; anything else is server text and stays hidden.
const FIXED_DETAIL_PREFIXES = ['the container', 'the pinned image', 'the runtime', 'the daemon', 'the deployment', 'not enough time', 'a container', 'service '];
function fixedDetail(detail: string): string {
  return FIXED_DETAIL_PREFIXES.some(p => detail.startsWith(p)) ? detail : '';
}
function explanationFor(result: Deployment['result']): string {
  const failing = result?.steps.find(s => s.outcome !== 'succeeded' && s.outcome !== 'skipped');
  if (!failing) return '';
  if (failing.outcome === 'denied' && failing.step === 'precondition') return 'A mapped container has configuration the definition does not describe. Review it on the host before planning again.';
  if (failing.outcome === 'unknown') return 'The host may or may not have acted. Inspect it before planning again.';
  if (failing.outcome === 'timed_out') return 'The host did not answer in time.';
  if (failing.outcome === 'failed') return 'A step failed on the host; the previous container may remain renamed with a .kyyard-prev suffix.';
  return '';
}

export function ApplicationDeploymentPlan(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close deployment plan' : 'Deployment plan'}</button>
    {open && <PlanView key={props.instanceID} {...props} />}
  </section>;
}
function isExpired(d: Deployment): boolean {
  return d.expired || Date.parse(d.expires_at) <= Date.now();
}
function ResultSection({ current }: { current: Deployment }) {
  const steps = usePagination(current.result?.steps ?? [], `${current.id}-steps`);
  const explanation = explanationFor(current.result);
  return <>
    <p>State {current.state}{current.settled_at ? `, settled ${new Date(current.settled_at).toLocaleString()}` : ''}.</p>
    {explanation && <p role="alert">{explanation}</p>}
    {current.result && <>
      {steps.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Step</th><th>Outcome</th><th>Detail</th></tr></thead><tbody>{steps.rows.map((s, i) => <tr key={`${s.service}-${s.step}-${i}`}>
        <td data-label="Service">{s.service}</td>
        <td data-label="Step">{s.step}</td>
        <td data-label="Outcome">{s.outcome}</td>
        <td data-label="Detail">{fixedDetail(s.detail)}</td>
      </tr>)}</tbody></table>
      {current.result.services.length > 0 && <ul className="ky-list">{current.result.services.map(s => <li key={s.container_id} style={{ overflowWrap: 'anywhere' }}><strong>{s.service}</strong><br /><span>{s.container_id}</span><br /><span>{s.image_id}</span></li>)}</ul>}
    </>}
  </>;
}
function PlanView({ base, instanceID }: Props) {
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
  const page = usePagination(current?.plan.services ?? [], base);
  const reload = () => { mapping.reload(); deployments.reload(); };
  const mismatched = mapping.data !== null && mapping.data.instance_id !== instanceID;

  const [applyConfirm, setApplyConfirm] = useState('');
  const [applyBusy, setApplyBusy] = useState(false);
  const [applyBlocked, setApplyBlocked] = useState(false);
  const [applyError, setApplyError] = useState('');
  useEffect(() => { setApplyConfirm(''); setApplyBusy(false); setApplyBlocked(false); setApplyError(''); }, [current?.id]);
  useEffect(() => {
    if (!current || current.state !== 'applying') return;
    let ticks = 0;
    const id = window.setInterval(() => {
      ticks += 1;
      if (ticks > 144) { window.clearInterval(id); return; }
      deployments.reload();
    }, 5000);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current?.id, current?.state]);

  const plan = async () => {
    if (!mapping.data) return;
    setBusy(true); setError([]);
    try {
      const r = await secureFetch(`${base}/deployments`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: mapping.data.instance_id, mapping_version: mapping.data.version, revision: mapping.data.preview.revision, confirm }) });
      if (r.ok) {
        const body: unknown = await r.json().catch(() => null);
        if (body && typeof body === 'object' && 'instance_id' in body) setPlanned(body as Deployment);
        else deployments.reload();
        setConfirm('');
        return;
      }
      setBlocked(true);
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const blockers = payload && typeof payload === 'object' && Array.isArray((payload as { blockers?: unknown }).blockers) ? ((payload as { blockers: unknown[] }).blockers.filter((b): b is keyof typeof messages => typeof b === 'string' && b in messages)) : [];
        setError(blockers.length ? blockers.map(b => messages[b]) : ['Ownership, mapping or the definition changed. Refresh applications and review before planning again.']);
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
        deployments.reload();
        return;
      }
      setApplyBlocked(true);
      if (r.status === 501) { setApplyError('Upgrade the host agent to enable deployments.'); return; }
      if (r.status === 403) { setApplyError('You do not have permission to apply deployments.'); return; }
      if (r.status === 409) {
        const payload: unknown = await r.json().catch(() => null);
        const code = payload && typeof payload === 'object' && 'code' in payload ? (payload as { code?: unknown }).code : undefined;
        setApplyError((typeof code === 'string' && APPLY_CODES[code]) || 'The apply was refused or its outcome is unknown. Refresh before trying again.');
        return;
      }
      setApplyError('The apply was refused or its outcome is unknown. Refresh before trying again.');
    } catch { setApplyBlocked(true); setApplyError('The outcome is unknown. Refresh before trying again.'); }
    finally { setApplyBusy(false); }
  };
  return <>
    <p>A plan records the exact revision, mapping and image identities a deployment would use. It is not executed here: no containers change, no images are pulled and no secret values are read. Runtime configuration remains unverified.</p>
    <StateNotice state={deployments.state} onRetry={reload} />
    {deployments.state === 'ready' && <>
      {mismatched && <p role="alert">Adoption changed. Refresh applications before planning.</p>}
      {!mismatched && (current ? <>
        <p>Plan for revision {current.revision}, mapping version {current.mapping_version}, project <bdi>{current.plan.project}</bdi>. {isExpired(current) ? 'This plan has expired; plan again to continue.' : `Expires ${new Date(current.expires_at).toLocaleString()}.`} It remains inert on its own; only planning again replaces it.</p>
        {page.controls}
        <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Pinned image</th><th>Replaces container</th><th>Secrets</th></tr></thead><tbody>{page.rows.map(s => <tr key={s.name}>
          <td data-label="Service"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.reference} · restart {s.restart || 'default'}</small></div></td>
          <td data-label="Pinned image"><div className="ky-resource-name"><span>{s.image_id}</span><small>{s.image_digest || 'No repository digest reported'}</small></div></td>
          <td data-label="Replaces container"><div className="ky-resource-name"><span>{s.container_id}</span><small>image {s.replaces.image_id}</small></div></td>
          <td data-label="Secrets">{s.secret_refs.length ? `${s.secret_refs.length} reference(s), values not shown` : 'None'}</td>
        </tr>)}</tbody></table>
      </> : <p>No plan for this instance.</p>)}
      {!mismatched && current && current.state === 'planned' && !isExpired(current) && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void apply(); }}>
        <p>Applying replaces the mapped containers on {current.endpoint_id} with revision {current.revision} of {current.plan.project}. Nothing rolls back on failure; a failed run leaves the previous container renamed on the host.</p>
        <label>Confirm apply project<input value={applyConfirm} onChange={e => setApplyConfirm(e.target.value)} disabled={applyBusy || applyBlocked} autoComplete="off" /></label>
        <button disabled={applyBusy || applyBlocked || applyConfirm !== current.plan.project}>Apply deployment</button>
        {applyError && <p role="alert">{applyError}</p>}
      </form>}
      {!mismatched && current && current.state !== 'planned' && <ResultSection current={current} />}
    </>}
    {mapping.state === 'ready' && !mismatched && mapping.data && <form className="dr-stack" onSubmit={e => { e.preventDefault(); void plan(); }}>
      <p>Planning replaces any earlier plan for this instance. Type the project name <bdi>{mapping.data.preview.project}</bdi> to confirm planning revision {mapping.data.preview.revision}. Nothing runs.</p>
      <label>Confirm plan project<input value={confirm} onChange={e => setConfirm(e.target.value)} disabled={busy || blocked} autoComplete="off" /></label>
      <button disabled={busy || blocked || confirm !== mapping.data.preview.project}>Plan deployment</button>
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => { setBlocked(false); setError([]); setPlanned(null); reload(); }}>Refresh plan</button>
      {error.length > 0 && <ul role="alert">{error.map(e => <li key={e}>{e}</li>)}</ul>}
    </form>}
    {mapping.state !== 'ready' && <StateNotice state={mapping.state} onRetry={mapping.reload} />}
  </>;
}
