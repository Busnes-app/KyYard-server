import { useEffect, useState } from 'react';
import { tenantWrite, useTenantResource, type UpdateCheck } from '../tenant';
import { secureFetch } from '../api';
import { StateNotice } from './StateNotice';
import { knownBlockers, messages } from './ApplicationPreflight';

const verdictText: Record<string, string> = { current: 'Up to date', update_available: 'Update available', pinned: 'Pinned', unknown_local: 'Unknown on host', registry_error: 'Registry error' };
export const detailText: Record<string, string> = {
  not_configured: 'No registry entry for this host and anonymous pulls are off.',
  unauthorized: 'The registry refused the credentials.',
  not_found: 'The image was not found in the registry.',
  rate_limited: 'The registry rate limit was reached; try later.',
  private_destination: 'The registry is on a private address this organization may not reach.',
  unavailable: 'The registry could not be reached.',
};
const texts = {
  forbidden: 'Only administrators and developers can check for updates.',
  conflict: {
    check_in_progress: 'An update check or update plan is already running.',
    mapping_required: 'Map the services first.',
    adoption_changed: 'Adoption changed. Refresh applications before checking.',
  },
};
export const planBlockers: Record<string, string> = {
  ...messages,
  update_not_mapped: 'Map the services first.',
  ...Object.fromEntries(Object.entries(detailText).map(([k, v]) => [`registry_${k}`, v])),
};
const planCodes: Record<number, string> = {
  403: 'You do not have permission to plan deployments.',
  429: 'Too many registry checks are running; try again in a moment.',
};
const planConflicts: Record<string, string> = {
  adoption_changed: 'Adoption changed. Refresh applications before planning.',
  deployment_in_progress: 'A deployment is being applied; wait for it to finish.',
  check_in_progress: 'An update check or update plan is already running.',
};
const planUnknown = 'The plan was refused or its outcome is unknown. Refresh before trying again.';
// Server strings never render raw: unknown verdicts and details show nothing.
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';
const short = (d: string) => /^sha256:[0-9a-f]{64}$/.test(d) ? d.slice(7, 19) : '—';

type Props = { base: string; instanceID: string; mappingVersion: number; project: string; latestRevision: number; onPlanned: () => void };

export function ApplicationUpdates(props: Props) {
  const [open, setOpen] = useState(false);
  if (props.mappingVersion < 1) return null;
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? 'Close updates' : 'Updates'}</button>
    {open && <UpdatesView key={`${props.base}/${props.instanceID}`} {...props} />}
  </section>;
}
function UpdatesView({ base, instanceID, project, latestRevision, onPlanned }: Props) {
  const checks = useTenantResource<UpdateCheck>(`${base}/updates`);
  // The last ready read stays on screen while a post-check reload is in flight.
  const [shown, setShown] = useState<UpdateCheck | null>(null);
  useEffect(() => { if (checks.state === 'ready') setShown(checks.data); }, [checks.state, checks.data]);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const check = async () => {
    setBusy(true);
    const err = await tenantWrite(`${base}/updates/check`, 'POST', undefined, texts);
    setBusy(false);
    setMessage(err);
    if (!err) checks.reload();
  };
  const [confirm, setConfirm] = useState('');
  const [planning, setPlanning] = useState(false);
  const [planMessages, setPlanMessages] = useState<string[]>([]);
  const [planned, setPlanned] = useState(false);
  const planUpdate = async (mappingVersion: number, update: string[]) => {
    setPlanning(true); setPlanMessages([]); setPlanned(false);
    try {
      const r = await secureFetch(`${base}/deployments`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ instance_id: instanceID, mapping_version: mappingVersion, revision: latestRevision, confirm, update }) });
      if (r.ok) { setConfirm(''); setPlanned(true); setPlanMessages(['Plan created; review it in the deployment plan panel.']); onPlanned(); return; }
      const body: unknown = r.status === 409 ? await r.json().catch(() => null) : null;
      const blockers = knownBlockers(body, planBlockers);
      const code = body && typeof body === 'object' ? (body as { code?: unknown }).code : undefined;
      const conflict = typeof code === 'string' ? fixed(planConflicts, code) : '';
      setPlanMessages(blockers.length ? blockers.map((b) => planBlockers[b]) : [conflict || (planCodes[r.status] ?? planUnknown)]);
    } catch { setPlanMessages([planUnknown]); }
    finally { setPlanning(false); }
  };
  const visible = checks.state === 'ready' || (checks.state === 'loading' && shown !== null);
  const data = checks.state === 'ready' ? checks.data : shown;
  const rows = data?.instance_id === instanceID ? data.services : [];
  const update = rows.filter((r) => r.verdict === 'update_available').map((r) => r.service);
  return <>
    <p>Compares each mapped service's image with its registry. Nothing is pulled or deployed.</p>
    <button type="button" className="btn-secondary" disabled={busy} onClick={() => void check()}>Check for updates</button>
    {message && <p role="alert">{message}</p>}
    {!visible && <StateNotice state={checks.state} onRetry={checks.reload} />}
    {visible && (rows.length === 0 ? <p>No update check yet.</p> : <table>
      <thead><tr><th>Service</th><th>Reference</th><th>Status</th><th>On host</th><th>In registry</th><th>Checked</th></tr></thead>
      <tbody>{rows.map((r) => <tr key={r.service}>
        <td>{r.service}</td>
        <td>{r.reference}</td>
        <td><span className="badge">{fixed(verdictText, r.verdict)}</span>{r.verdict === 'registry_error' && fixed(detailText, r.detail) && <div>{fixed(detailText, r.detail)}</div>}</td>
        <td><code>{short(r.local_digest)}</code></td>
        <td><code>{short(r.remote_digest)}</code></td>
        <td>{new Date(r.checked_at).toLocaleString()}</td>
      </tr>)}</tbody>
    </table>)}
    {data && update.length > 0 && <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void planUpdate(data.mapping_version, update); }}>
      <p>Planning pins each available update to its registry digest and replaces any earlier plan for this instance. Type the project name <bdi>{project}</bdi> to confirm. Nothing runs until the plan is applied.</p>
      <label>Confirm update project<input value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={planning} autoComplete="off" /></label>
      <button disabled={planning || confirm !== project}>Plan update</button>
    </form>}
    {planMessages.length > 0 && <ul role={planned ? 'status' : 'alert'}>{planMessages.map((m) => <li key={m}>{m}</li>)}</ul>}
  </>;
}
