import { useEffect, useRef, useState, type ReactNode } from 'react';
import { secureFetch } from '../api';
import type { DirectCommand, WorkloadConfiguration, WorkloadContainer, WorkloadRef } from '../tenant';
import { RESULT_CODES, StepTable } from './ApplicationDeploymentPlan';
import { diffWorkload, parseWorkloadConfiguration, toWorkloadSpec, workloadUnsupportedLabel, type WorkloadSpec } from './workloadConfiguration';
import { Group, int, Lines, Num, Text } from './configurationGroups/fields';
import { EnvRows } from './configurationGroups/environment';
import { NOT_FOUND, NOT_GRANTED, UPGRADE_CLUSTER, WORKLOAD_STEPS, workloadRefusal } from './workloadTexts';

export const KIND_NAMES: Record<string, string> = { deployment: 'Deployment', statefulset: 'StatefulSet', daemonset: 'DaemonSet' };
export const workloadURL = (base: string, t: WorkloadRef) => `${base}/workloads/${encodeURIComponent(t.namespace)}/${encodeURIComponent(t.kind)}/${encodeURIComponent(t.name)}`;

const argv = (items: string[]) => { const out = [...items]; while (out.length && out[out.length - 1] === '') out.pop(); return out; };
const canonical = (c: WorkloadContainer): WorkloadContainer => ({
  ...c, command: argv(c.command), args: argv(c.args), resources: { ...c.resources },
  env: c.env.map((e) => e.secret_ref ? { name: e.name, secret_ref: e.secret_ref } : e.config_map_ref ? { name: e.name, config_map_ref: e.config_map_ref } : { name: e.name, value: e.value ?? '' }),
});
// build is the one canonical shape, so the read and an untouched draft have no diff.
const build = (s: WorkloadSpec): WorkloadSpec => ({ ...s, containers: s.containers.map(canonical), init_containers: s.init_containers.map(canonical) });
const RESOURCES = [['cpu_request', 'CPU request'], ['cpu_limit', 'CPU limit'], ['memory_request', 'Memory request'], ['memory_limit', 'Memory limit']] as const;

// onReread asks the owner for a fresh read; conflict is a settled apply refused as stale.
type FormProps = { base: string; initial: WorkloadConfiguration; onSent: (command: DirectCommand) => void; pending?: boolean; onReread?: () => void; conflict?: boolean };

// WorkloadConfigurationForm edits replicas and each container's image, command, arguments,
// literal environment and resources. Init containers, strategy and references pass through as
// read. Save applies at the read's resource_version, so a workload changed since is refused.
export function WorkloadConfigurationForm({ base, initial, onSent, pending = false, onReread, conflict = false }: FormProps) {
  const target = initial.target;
  const start = build(toWorkloadSpec(initial));
  const [draft, setDraft] = useState<WorkloadSpec>(start);
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [lost, setLost] = useState(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const spec = build(draft);
  const changes = diffWorkload(start, spec);
  const badReplicas = spec.replicas !== undefined && !(Number.isInteger(spec.replicas) && spec.replicas >= 0 && spec.replicas <= 1000);
  const incomplete = spec.containers.some((c) => c.image.trim() === '' || c.env.some((e) => e.name === ''));
  const ready = !badReplicas && !incomplete && spec.unsupported.length === 0 && changes.length > 0 && confirm === target.name;
  const setContainer = (i: number, patch: Partial<WorkloadContainer>) => setDraft((d) => ({ ...d, containers: d.containers.map((c, j) => j === i ? { ...c, ...patch } : c) }));
  const submit = async () => {
    setBusy(true); setError('');
    try {
      const resp = await secureFetch(`${workloadURL(base, target)}/apply`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resource_version: initial.resource_version, spec, confirm }) });
      if (!alive.current) return;
      if (!resp.ok) { const text = await workloadRefusal(resp); if (alive.current) setError(text); return; }
      const cmd: DirectCommand = await resp.json();
      if (!alive.current) return;
      setConfirm(''); onSent(cmd);
    } catch {
      if (alive.current) { setLost(true); setError("Connection lost. The change may have been sent. Check the workload's Activity tab and read the workload again before retrying."); }
    } finally { if (alive.current) setBusy(false); }
  };
  return <section className="ky-config-form" aria-label="Edit workload">
    {spec.unsupported.length > 0 && <div className="dr-alert dr-alert-warn">
      <p>This workload has settings KyYard cannot carry over, so saving is disabled. The workload:</p>
      <ul className="ky-list">{spec.unsupported.map((c) => <li key={c}>{workloadUnsupportedLabel(c)}</li>)}</ul>
    </div>}
    <fieldset className="ky-config-groups" disabled={busy || pending}>
      {initial.replicas !== undefined && <Group title="Scale"><Num label="Replicas" value={initial.replicas} onChange={(v) => setDraft((d) => ({ ...d, replicas: v.trim() === '' ? NaN : int(v) }))} /></Group>}
      {draft.containers.map((c, i) => <Group key={c.name} title={`Container ${c.name}`}>
        <Text label={`Image of ${c.name}`} value={c.image} onChange={(image) => setContainer(i, { image })} />
        <Lines label={`Command of ${c.name}`} value={c.command} onChange={(command) => setContainer(i, { command })} />
        <Lines label={`Arguments of ${c.name}`} value={c.args} onChange={(args) => setContainer(i, { args })} />
        <Group title={`Environment of ${c.name}`}><EnvRows noun={`${c.name} variable`} env={c.env} blank={{ name: '', value: '' }} onChange={(env) => setContainer(i, { env })} /></Group>
        {RESOURCES.map(([key, name]) => <Text key={key} label={`${name} of ${c.name}`} placeholder="unset" value={c.resources[key]} onChange={(v) => setContainer(i, { resources: { ...c.resources, [key]: v } })} />)}
      </Group>)}
    </fieldset>
    <div className="ky-config-save">
      <p>Saving updates this {KIND_NAMES[target.kind] ?? 'workload'} in place and Kubernetes replaces its pods. Nothing rolls back.</p>
      <p>{changes.length ? `Changes: ${changes.join(', ')}` : 'No changes.'}</p>
      {badReplicas && <p>Enter replicas as a whole number from 0 to 1000.</p>}
      {incomplete && <p>Every container needs an image and every variable a name.</p>}
      <label>{`Type the workload name ${target.name} to confirm`}<input value={confirm} autoComplete="off" onChange={(e) => setConfirm(e.target.value)} /></label>
      <div><button type="button" disabled={!ready || busy || pending || lost} onClick={() => void submit()}>Save and apply</button>
        {(lost || conflict) && onReread && <button type="button" className="btn-secondary" onClick={onReread}>Read again</button>}</div>
    </div>
    {error && <p role="alert" className="dr-alert dr-alert-error">{error}</p>}
  </section>;
}

const OUTCOMES: Record<string, string> = {
  succeeded: 'Applied.',
  failed: 'The cluster did not complete the change; the steps say which. Nothing was rolled back.',
  denied: 'The cluster refused the change; nothing was applied.',
  unknown: 'The outcome is unknown: the agent may or may not have applied it. Read the workload again before retrying.',
};
// WorkloadResult is a settled apply: outcome, result code and the step table in workload words.
export function WorkloadResult({ command }: { command: DirectCommand }) {
  return <div role="status">
    <p>{Object.hasOwn(OUTCOMES, command.outcome) ? OUTCOMES[command.outcome] : 'Unrecognised outcome.'}</p>
    {command.result?.code && Object.hasOwn(RESULT_CODES, command.result.code) && <p>{RESULT_CODES[command.result.code]}</p>}
    {command.result && command.result.steps.length > 0 && <StepTable steps={command.result.steps} texts={WORKLOAD_STEPS} />}
  </div>;
}

const READ_ERRORS: Record<number, string> = {
  403: 'You do not have permission to edit this workload.',
  404: NOT_FOUND,
  422: NOT_GRANTED,
  429: 'Too many configuration requests. Wait a minute and try again.',
  501: UPGRADE_CLUSTER,
  504: 'The cluster agent did not answer in time. Try again.',
};
const READ_CONFLICTS: Record<string, string> = {
  runtime_unsupported: "This endpoint's runtime does not support this action.",
  endpoint_offline: 'The cluster agent is not connected. Reconnect it to edit this workload.',
};
type Read = { kind: 'loading' } | { kind: 'ready'; data: WorkloadConfiguration } | { kind: 'managed' } | { kind: 'error'; text: string };

// EditWorkload reads the configuration (literal environment values included) once per mount and
// hands it to the form; a read the server refuses as application-managed renders managed.
export function EditWorkload({ base, target, pending, onSent, managed, conflict = false }: { base: string; target: WorkloadRef; pending: boolean; onSent: (command: DirectCommand) => void; managed: ReactNode; conflict?: boolean }) {
  const [attempt, setAttempt] = useState(0);
  const [read, setRead] = useState<Read>({ kind: 'loading' });
  const { namespace, kind, name } = target;
  useEffect(() => {
    const controller = new AbortController();
    const ref = { namespace, kind, name };
    setRead({ kind: 'loading' });
    (async () => {
      try {
        const r = await fetch(`${workloadURL(base, ref)}/configuration`, { signal: controller.signal, cache: 'no-store' });
        if (controller.signal.aborted) return;
        if (!r.ok) {
          const code = r.status === 409 ? ((await r.json().catch(() => ({}))) as { code?: unknown }).code : undefined;
          if (controller.signal.aborted) return;
          if (code === 'application_managed') { setRead({ kind: 'managed' }); return; }
          const text = typeof code === 'string' && Object.hasOwn(READ_CONFLICTS, code) ? READ_CONFLICTS[code] ?? ''
            : r.status === 409 ? 'The cluster agent changed during the read. Read again.' : READ_ERRORS[r.status] ?? 'The configuration is unavailable right now.';
          setRead({ kind: 'error', text });
          return;
        }
        const data = parseWorkloadConfiguration(await r.json(), ref);
        if (!controller.signal.aborted) setRead(data ? { kind: 'ready', data } : { kind: 'error', text: 'The configuration did not match this workload. Refresh the inventory.' });
      } catch { if (!controller.signal.aborted) setRead({ kind: 'error', text: 'The configuration connection was lost.' }); }
    })();
    return () => controller.abort();
  }, [base, namespace, kind, name, attempt]);
  if (read.kind === 'managed') return <>{managed}</>;
  return <section className="panel" aria-label="Configuration">
    {read.kind === 'loading' ? <p role="status">Reading configuration…</p>
      : read.kind === 'error' ? <><p role="status">{read.text}</p><button type="button" className="btn-secondary" onClick={() => setAttempt((n) => n + 1)}>Read again</button></>
      : <WorkloadConfigurationForm base={base} initial={read.data} pending={pending} onSent={onSent} conflict={conflict} onReread={() => setAttempt((n) => n + 1)} />}
  </section>;
}
