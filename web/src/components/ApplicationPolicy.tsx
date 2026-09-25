import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type PolicyRun, type UpdatePolicy } from '../tenant';
import { StateNotice } from './StateNotice';
import { detailText, planBlockers } from './ApplicationUpdates';

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MODES: Record<string, string> = { apply: 'Plan and apply', plan_only: 'Plan only; apply by hand' };
export const RUN_OUTCOMES: Record<string, string> = {
  '': 'Running',
  skipped_missed: 'Skipped: server not running',
  skipped_busy: 'Skipped: busy',
  no_update: 'No update',
  planned: 'Planned; waiting to be applied',
  applied: 'Applied',
  blocked: 'Blocked',
  failed: 'Failed',
  paused: 'Paused the policy',
};
// The scheduler's own sentences, as a run's detail or a pause reason carries them.
const SENTENCES: Record<string, string> = {
  'the server was not running during this window': 'The server was not running during this window.',
  'another deployment occupied the window': 'Another deployment or plan occupied the window.',
  "the policy's creator no longer holds application.deploy": 'The administrator who last saved this policy can no longer deploy. Save the policy to make it act as you, then resume it.',
  'the server restarted during the run': 'The server restarted during the run.',
  'three consecutive windows failed': 'Three windows in a row failed. Fix the cause, then resume.',
};
const CODES: Record<string, string> = {
  ...detailText,
  not_adopted: 'The application is no longer adopted.',
  mapping_required: 'Map the services first.',
  adoption_changed: 'Adoption or inventory changed during the run.',
  deployment_in_progress: 'A deployment was being applied.',
  endpoint_offline: 'The endpoint was not connected.',
  not_sent: 'The endpoint disconnected before the deployment was sent.',
  check_in_progress: 'An update check or update plan was already running.',
  too_many_checks: 'Too many registry checks were running.',
  invalid: 'The run was refused as invalid.',
  error: 'The run failed on the server; check the server log.',
};
const INVALID: Record<string, string> = {
  invalid_mode: 'Choose a mode.',
  invalid_timezone: 'The server does not recognise this time zone. Choose another.',
  invalid_weekdays: 'Choose at least one day.',
  invalid_window: 'The window must end after it starts, last at least 15 minutes and end by midnight; windows cannot cross midnight.',
};
// Server strings never render raw: unknown keys show nothing.
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';
const hhmm = (m: number) => `${String(Math.floor(m / 60) % 24).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
const minutes = (v: string) => { const [h, m] = v.split(':').map(Number); return h * 60 + m; };

export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone;
// hasZoneList is false on an engine without Intl.supportedValuesOf; the editor falls back to a
// typed zone rather than throwing.
export const hasZoneList = () => typeof Intl.supportedValuesOf === 'function';
// zoneChoices is the browser's zone list plus the zones in keep: a zone the server accepts but
// this browser does not list (UTC, aliases) must never be swapped silently. Without
// Intl.supportedValuesOf the list is only the kept and browser zones.
export function zoneChoices(keep: string[]): string[] {
  const known = hasZoneList() ? Intl.supportedValuesOf('timeZone') : [];
  return Array.from(new Set([...keep, browserZone(), ...known].filter(Boolean)));
}
// formatIn shows an instant in zone, or in UTC when this browser does not know the zone.
export function formatIn(iso: string, zone: string): string {
  try {
    return new Intl.DateTimeFormat(undefined, { timeZone: zone, weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso));
  } catch {
    return `${iso} (UTC)`;
  }
}
// runDetail renders a stored detail through the fixed tables only.
export function runDetail(run: PolicyRun): string {
  if (run.outcome === 'blocked') return run.detail.split(',').map((b) => fixed(planBlockers, b)).filter(Boolean).join(' ');
  return fixed(SENTENCES, run.detail) || fixed(CODES, run.detail);
}
function problem(days: number[], start: number, end: number): string {
  if (days.length === 0) return INVALID.invalid_weekdays;
  if (!(end - start >= 15)) return INVALID.invalid_window;
  return '';
}

type Props = { base: string; admin: boolean };

export function ApplicationPolicy(props: Props) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close update policy' : 'Update policy'}</button>
    {open && <PolicyView key={props.base} {...props} />}
  </section>;
}

function PolicyView({ base, admin }: Props) {
  const url = `${base}/update-policy`;
  const policy = useTenantResource<UpdatePolicy>(url);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const write = async (method: string, path: string, body?: unknown) => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(path, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
      if (r.ok) { policy.reload(); return; }
      const payload: unknown = await r.json().catch(() => null);
      const code = payload && typeof payload === 'object' ? (payload as { code?: unknown }).code : undefined;
      if (r.status === 400) setMessage((typeof code === 'string' && fixed(INVALID, code)) || 'The policy was refused as invalid.');
      else if (r.status === 403) setMessage('Only organization administrators can change update policies.');
      else if (r.status === 409 && code === 'policy_not_paused') setMessage('The policy is not paused.');
      else if (r.status === 404) setMessage('The policy or the application no longer exists. Refresh.');
      else setMessage('The change was refused or its outcome is unknown. Refresh before trying again.');
    } catch { setMessage('Offline: the server could not be reached.'); }
    finally { setBusy(false); }
  };
  if (policy.state !== 'ready' && policy.state !== 'notfound') return <StateNotice state={policy.state} onRetry={policy.reload} />;
  const p = policy.state === 'ready' ? policy.data : null;
  return <>
    <p>A policy checks this application's images in a weekly maintenance window and plans, or plans and applies, what changed, acting as the administrator who last saved it. Every run is audited.</p>
    {p ? <PolicyStatus policy={p} admin={admin} busy={busy} onResume={() => void write('POST', `${url}/resume`)} /> : <p>No update policy.</p>}
    {admin && <PolicyEditor key={p?.updated_at ?? 'new'} policy={p} busy={busy} onSave={(body) => void write('PUT', url, body)} onDelete={() => { if (window.confirm('Delete this update policy and its run history? Deployments it made are kept.')) void write('DELETE', url); }} />}
    {message && <p role="alert">{message}</p>}
    {p && <RunList runs={p.runs ?? []} zone={p.timezone} />}
  </>;
}

function PolicyStatus({ policy, admin, busy, onResume }: { policy: UpdatePolicy; admin: boolean; busy: boolean; onResume: () => void }) {
  const schedule = `${policy.weekdays.map((d) => DAYS[d] ?? '').join(', ')} ${hhmm(policy.start_minute)}–${hhmm(policy.end_minute)} (${policy.timezone})`;
  return <div className="dr-stack">
    <p>{fixed(MODES, policy.mode)} · {schedule}</p>
    {policy.status === 'paused' ? <>
      <p role="status">Paused. {fixed(SENTENCES, policy.paused_reason)}</p>
      {admin && <button type="button" disabled={busy} onClick={onResume}>Resume policy</button>}
    </> : <p role="status">Active. {policy.next_occurrence ? `Next window: ${formatIn(policy.next_occurrence, policy.timezone)} (your time: ${formatIn(policy.next_occurrence, browserZone())}).` : 'No upcoming window.'}{policy.consecutive_failures > 0 ? ` ${policy.consecutive_failures} failed window(s) in a row; the policy pauses at 3.` : ''}</p>}
  </div>;
}

function PolicyEditor({ policy, busy, onSave, onDelete }: { policy: UpdatePolicy | null; busy: boolean; onSave: (body: unknown) => void; onDelete: () => void }) {
  const [mode, setMode] = useState(policy?.mode ?? 'plan_only');
  const [days, setDays] = useState<number[]>(policy?.weekdays ?? [1, 2, 3, 4, 5]);
  const [start, setStart] = useState(hhmm(policy?.start_minute ?? 120));
  const [end, setEnd] = useState(hhmm(policy?.end_minute ?? 240));
  const [zone, setZone] = useState(policy?.timezone ?? browserZone());
  const [error, setError] = useState('');
  const toggle = (d: number) => setDays(days.includes(d) ? days.filter((x) => x !== d) : [...days, d].sort((a, b) => a - b));
  const submit = () => {
    const s = minutes(start), e = minutes(end) || 1440; // 00:00 as the end is midnight
    const p = problem(days, s, e);
    setError(p);
    if (!p) onSave({ mode, timezone: zone, weekdays: days, start_minute: s, end_minute: e });
  };
  return <form className="dr-stack" onSubmit={(ev) => { ev.preventDefault(); submit(); }}>
    <label>Mode<select value={mode} onChange={(e) => setMode(e.target.value)} disabled={busy}>{Object.entries(MODES).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</select></label>
    <fieldset><legend>Days</legend>{DAYS.map((name, d) => <label key={name}><input type="checkbox" checked={days.includes(d)} onChange={() => toggle(d)} disabled={busy} />{name}</label>)}</fieldset>
    <label>Window start<input type="time" value={start} onChange={(e) => setStart(e.target.value)} required disabled={busy} /></label>
    <label>Window end<input type="time" value={end} onChange={(e) => setEnd(e.target.value)} required disabled={busy} /></label>
    <p>Times are wall-clock times in the chosen zone. A window lasts at least 15 minutes and cannot cross midnight; an end of 00:00 means midnight.</p>
    <label>Time zone<select value={zone} onChange={(e) => setZone(e.target.value)} disabled={busy}>{zoneChoices([policy?.timezone ?? '', zone]).map((z) => <option key={z} value={z}>{z}</option>)}</select></label>
    {!hasZoneList() && <label>Other timezone<input type="text" value={zone} onChange={(e) => setZone(e.target.value)} disabled={busy} placeholder="e.g. Europe/Paris" /></label>}
    {error && <p role="alert">{error}</p>}
    <button disabled={busy}>Save policy</button>
    {policy && <button type="button" className="btn-danger" disabled={busy} onClick={onDelete}>Delete policy</button>}
  </form>;
}

function RunList({ runs, zone }: { runs: PolicyRun[]; zone: string }) {
  if (runs.length === 0) return <p>No runs yet.</p>;
  return <table>
    <thead><tr><th>Window</th><th>Outcome</th><th>Detail</th><th>Deployment</th></tr></thead>
    <tbody>{runs.map((r) => <tr key={r.id}>
      <td>{formatIn(r.occurrence, zone)}</td>
      <td><span className="badge">{Object.hasOwn(RUN_OUTCOMES, r.outcome) ? RUN_OUTCOMES[r.outcome] : 'Unrecognised outcome'}</span></td>
      <td>{runDetail(r)}</td>
      <td>{/^[0-9a-f-]{36}$/.test(r.deployment_id) ? <code title={r.deployment_id}>{r.deployment_id.slice(0, 8)}</code> : '—'}</td>
    </tr>)}</tbody>
  </table>;
}
