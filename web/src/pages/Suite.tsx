import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { secureFetch } from '../api';
import { useTenantResource } from '../tenant';
import { EmptyNotice, StateNotice } from '../components/StateNotice';

// Wire shapes of internal/suite (runner protocol v1) as /api/suite/* returns them.
interface Target { name: string; kind: string }
interface InstalledApp { name: string; category: string; version: string; target: string; state: string; health: { ok: boolean; checked: string } | null; last_result: { step: string; status: string; time: string } | null }
export interface SuiteStatus { configured: boolean; runner?: { release_set: string; runner_id: string; plan_revision: number; plan_owner: string }; operation?: string | null; targets?: Target[]; apps?: InstalledApp[] }
interface CatalogApp { name: string; description: string; category: string; version: string; depends_on: string[]; installed: boolean; upgrade: { min_from: string; no_skip: string[]; migrates_data: boolean } }
interface Catalog { release_set: string; apps: CatalogApp[] }
interface Step { id: string; status: string; time: string; diagnostics: string }
interface Operation { id: string; actor_id: string; kind: Kind; apps: string[]; target: string; state: string; started: string; finished: string | null; error_code: string; operation?: { steps: Step[] | null; error: { code: string; message: string } | null } }
type Kind = 'install' | 'uninstall' | 'unmanage' | 'upgrade';
interface Action { kind: Kind; app?: string }

const when = (t: string | null | undefined) => (t ? new Date(t).toLocaleString() : '—');
const stateBadge: Record<string, string> = { running: 'badge-accent', succeeded: 'badge-success', ok: 'badge-success', failed: 'badge-danger', unknown: 'badge-warning', skipped: 'badge-secondary' };
const Badge = ({ value }: { value: string }) => <span className={`badge ${stateBadge[value] ?? 'badge-secondary'}`}>{value}</span>;

// A refusal shows the server's or runner's code and its message, which the runner redacts.
async function refusal(r: Response): Promise<string> {
  const body = await r.json().catch(() => null) as { error?: unknown; code?: unknown } | null;
  if (body && typeof body.error === 'string' && typeof body.code === 'string') return `${body.error} (${body.code})`;
  if (r.status === 403) return 'Only a platform administrator can manage the suite.';
  return `Request refused (${r.status}).`;
}

export function Suite({ totp }: { totp: boolean }) {
  const status = useTenantResource<SuiteStatus>('/api/suite/status');
  return <div className="ky-page">
    <h1>Suite</h1>
    <p>Install, upgrade and remove Ky suite apps through the suite runner.</p>
    <StateNotice state={status.state} onRetry={status.reload} />
    {status.state === 'ready' && status.data && (status.data.configured
      ? <SuiteManager status={status.data} reload={status.reload} totp={totp} />
      : <section className="panel"><h2>Not configured</h2><p>No suite runner is configured, so there is nothing to manage here. KyQuickStart's hand-off (<code>kyquickstart handoff --kyyard-target &lt;target&gt;</code>) installs the runner beside KyYard and sets <code>KY_SUITE_RUNNER_SOCKET</code>.</p></section>)}
  </div>;
}

function SuiteManager({ status, reload, totp }: { status: SuiteStatus; reload: () => void; totp: boolean }) {
  const catalog = useTenantResource<Catalog>('/api/suite/catalog');
  const [action, setAction] = useState<Action | null>(null);
  const [started, setStarted] = useState<string | null>(null);
  const opID = started ?? status.operation ?? null;
  const operation = useOperation(opID, () => { reload(); catalog.reload(); });
  const busy = !!status.operation || operation.op?.state === 'running';
  const newest = new Map(catalog.data?.apps.map((a) => [a.name, a.version] as const));
  const installed = status.apps ?? [];
  const available = catalog.data?.apps.filter((a) => !a.installed) ?? [];
  const targets = status.targets ?? [];
  const runner = status.runner;
  return <>
    {runner && <p>Plan revision {runner.plan_revision} · owner {runner.plan_owner} · release set {runner.release_set}</p>}
    <section className="panel" aria-labelledby="suite-installed">
      <h2 id="suite-installed">Installed</h2>
      {installed.length === 0 ? <EmptyNotice>No suite apps are installed.</EmptyNotice> : <div style={{ overflowX: 'auto' }}>
        <table className="ky-table ky-responsive-table">
          <thead><tr><th>Name</th><th>Version</th><th>Newest</th><th>Target</th><th>Health</th><th>Last result</th><th><span className="sr-only">Actions</span></th></tr></thead>
          <tbody>{installed.map((a) => <tr key={a.name}>
            <td data-label="Name" style={{ whiteSpace: 'nowrap' }}>{a.name}</td>
            <td data-label="Version">{a.version}</td>
            <td data-label="Newest">{newest.get(a.name) ?? '—'}</td>
            <td data-label="Target">{a.target}</td>
            <td data-label="Health">{a.state !== 'installed' && <><Badge value={a.state} />{' '}</>}{a.health ? <span className={`badge ${a.health.ok ? 'badge-success' : 'badge-danger'}`} title={`Checked ${when(a.health.checked)}`}>{a.health.ok ? 'healthy' : 'unhealthy'}</span> : 'not checked'}</td>
            <td data-label="Last result">{a.last_result ? <><code>{a.last_result.step}</code> <Badge value={a.last_result.status} /> {when(a.last_result.time)}</> : '—'}</td>
            <td data-label="Actions"><span className="ky-container-actions">
              {(['upgrade', 'uninstall', 'unmanage'] as const).map((kind) => <button key={kind} className={kind === 'uninstall' ? 'btn-danger' : 'btn-secondary'} disabled={busy} onClick={() => setAction({ kind, app: a.name })} aria-label={`${kind[0].toUpperCase() + kind.slice(1)} ${a.name}`}>{kind[0].toUpperCase() + kind.slice(1)}</button>)}</span>
            </td>
          </tr>)}</tbody>
        </table>
      </div>}
    </section>
    <section className="panel" aria-labelledby="suite-upgrade">
      <h2 id="suite-upgrade">Upgrade suite</h2>
      <p>Upgrades every installed app to the release set this runner carries, one at a time in dependency order, stopping at the first failure.</p>
      <button disabled={busy || installed.length === 0} onClick={() => setAction({ kind: 'upgrade' })}>Upgrade suite</button>
    </section>
    <section className="panel" aria-labelledby="suite-available">
      <h2 id="suite-available">Available</h2>
      <StateNotice state={catalog.state} onRetry={catalog.reload} />
      {catalog.state === 'ready' && (available.length === 0 ? <EmptyNotice>Every catalog app is installed.</EmptyNotice> : <div style={{ overflowX: 'auto' }}>
        <table className="ky-table ky-responsive-table">
          <thead><tr><th>Name</th><th>Description</th><th>Version</th><th>Category</th><th>Depends on</th><th><span className="sr-only">Actions</span></th></tr></thead>
          <tbody>{available.map((a) => <tr key={a.name}>
            <td data-label="Name" style={{ whiteSpace: 'nowrap' }}>{a.name}</td>
            <td data-label="Description">{a.description}</td>
            <td data-label="Version">{a.version}</td>
            <td data-label="Category">{a.category}</td>
            <td data-label="Depends on">{a.depends_on.join(', ') || '—'}</td>
            <td data-label="Actions"><button className="btn-secondary" disabled={busy || targets.length === 0} onClick={() => setAction({ kind: 'install', app: a.name })} aria-label={`Install ${a.name}`}>Install</button></td>
          </tr>)}</tbody>
        </table>
      </div>)}
    </section>
    {opID && <OperationPanel {...operation} />}
    {action && <ActionDialog action={action} totp={totp} targets={targets} onClose={() => setAction(null)} onStarted={(id) => { setAction(null); setStarted(id); reload(); }} />}
  </>;
}

// useOperation reads the operation now and 1.5 s after each answer while it is running; a failed
// read stops until "Check again".
function useOperation(id: string | null, onSettled: () => void) {
  const [op, setOp] = useState<Operation | null>(null);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const settled = useRef(onSettled);
  useEffect(() => { settled.current = onSettled; });
  useEffect(() => {
    if (!id) return;
    const abort = new AbortController();
    let timer = 0;
    const poll = async () => {
      try {
        const r = await fetch(`/api/suite/operations/${encodeURIComponent(id)}`, { signal: abort.signal });
        if (!r.ok) { const text = await refusal(r); if (!abort.signal.aborted) setError(text); return; }
        const data: Operation = await r.json();
        if (abort.signal.aborted) return;
        setOp(data); setError('');
        if (data.state === 'running') timer = window.setTimeout(() => void poll(), 1500);
        else settled.current();
      } catch { if (!abort.signal.aborted) setError('Could not read the operation. Check again before starting another.'); }
    };
    void poll();
    return () => { abort.abort(); window.clearTimeout(timer); };
  }, [id, tick]);
  return { op: op?.id === id ? op : null, error, retry: () => setTick((n) => n + 1) };
}

function OperationPanel({ op, error, retry }: { op: Operation | null; error: string; retry: () => void }) {
  const failure = op?.operation?.error;
  return <section className="panel" aria-labelledby="suite-operation">
    <h2 id="suite-operation">Operation</h2>
    {op && <>
      <p>{op.kind} {op.apps.length ? op.apps.join(', ') : 'every installed app'}{op.target && ` on ${op.target}`} · <Badge value={op.state} /> · started {when(op.started)} · finished {when(op.finished)}</p>
      {op.state === 'running' && <p role="status">Running; this panel refreshes until the operation ends.</p>}
      {failure ? <p role="alert">{failure.message} ({failure.code})</p> : op.error_code && <p role="alert">Error code: {op.error_code}</p>}
      {op.operation?.steps && op.operation.steps.length > 0 && <div style={{ overflowX: 'auto' }}>
        <table className="ky-table ky-responsive-table">
          <thead><tr><th>Step</th><th>Status</th><th>Time</th><th>Diagnostics</th></tr></thead>
          <tbody>{op.operation.steps.map((s) => <tr key={s.id}>
            <td data-label="Step"><code>{s.id}</code></td>
            <td data-label="Status"><Badge value={s.status} /></td>
            <td data-label="Time">{when(s.time)}</td>
            <td data-label="Diagnostics">{s.diagnostics && <pre className="ky-log" style={{ maxHeight: 160 }}>{s.diagnostics}</pre>}</td>
          </tr>)}</tbody>
        </table>
      </div>}
    </>}
    {!op && !error && <p role="status">Loading operation…</p>}
    {error && <><p role="alert">{error}</p><button className="btn-secondary" onClick={retry}>Check again</button></>}
  </section>;
}

const descriptions: Record<Kind, (app?: string) => string> = {
  install: (app) => `Installs ${app} and any dependency it is missing on the chosen target.`,
  uninstall: (app) => `Removes the workloads of ${app}. Its data and secrets stay on the target. Refused while an installed app depends on it.`,
  unmanage: (app) => `Stops managing ${app}: its KyQuickStart labels are removed and it leaves the plan. It keeps running.`,
  upgrade: (app) => app ? `Upgrades ${app} to the version this runner's release set carries.` : 'Upgrades every installed app, one at a time in dependency order, stopping at the first failure.',
};

function ActionDialog({ action, totp, targets, onClose, onStarted }: { action: Action; totp: boolean; targets: Target[]; onClose: () => void; onStarted: (id: string) => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { dialog.current?.showModal(); }, []);
  const expected = action.app ?? 'suite';
  const title = `${action.kind[0].toUpperCase() + action.kind.slice(1)} ${expected}`;
  const [confirm, setConfirm] = useState('');
  const [target, setTarget] = useState(targets[0]?.name ?? '');
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    const body: Record<string, unknown> = { kind: action.kind, apps: action.app ? [action.app] : [], confirm, password: String(data.get('password') ?? '') };
    if (totp) body.code = String(data.get('code') ?? '');
    if (action.kind === 'install') body.target = target;
    if (action.kind === 'upgrade') body.acknowledge_no_backup = ack;
    // The password and code never outlive the request.
    for (const name of ['password', 'code']) { const input = form.elements.namedItem(name); if (input instanceof HTMLInputElement) input.value = ''; }
    setBusy(true); setError('');
    try {
      const r = await secureFetch('/api/suite/operations', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (r.ok) { const op: Operation = await r.json(); onStarted(op.id); return; }
      setError(await refusal(r));
    } catch { setError('Connection lost. The operation may have started; reload the page before trying again.'); }
    setBusy(false);
  };
  return <dialog ref={dialog} className="modal-window" aria-labelledby="suite-dialog-title" onCancel={(e) => { e.preventDefault(); onClose(); }} onClose={onClose}>
    <form className="dr-stack" onSubmit={(e) => void submit(e)}>
      <h2 id="suite-dialog-title">{title}</h2>
      <p>{descriptions[action.kind](action.app)}</p>
      {action.kind === 'install' && <label>Target<select value={target} onChange={(e) => setTarget(e.target.value)} disabled={busy} required>{targets.map((t) => <option key={t.name} value={t.name}>{t.name} ({t.kind})</option>)}</select></label>}
      {action.kind === 'upgrade' && <label><input type="checkbox" style={{ width: 'auto' }} checked={ack} onChange={(e) => setAck(e.target.checked)} disabled={busy} /> No backup acknowledgement: I accept upgrading without a verified backup. Upgrades that migrate data are refused without a verified pre-upgrade backup; this does not override that.</label>}
      <label>Type <bdi>{expected}</bdi> to confirm<input value={confirm} onChange={(e) => setConfirm(e.target.value)} disabled={busy} autoComplete="off" /></label>
      <label>Current password<input name="password" type="password" autoComplete="current-password" required maxLength={1024} disabled={busy} /></label>
      {totp && <label>Authentication code<input name="code" inputMode="numeric" autoComplete="one-time-code" required maxLength={64} disabled={busy} /></label>}
      {error && <p role="alert">{error}</p>}
      <div style={{ display: 'flex', gap: 8 }}>
        <button type="submit" disabled={busy || confirm !== expected}>{title}</button>
        <button type="button" className="btn-secondary" onClick={onClose} disabled={busy}>Cancel</button>
      </div>
    </form>
  </dialog>;
}
