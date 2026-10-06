import { useEffect, useRef, useState, type ReactNode } from 'react';
import { secureFetch } from '../api';
import { dnsLabel } from '../router';
import type { DirectCommand, WorkloadConfiguration, WorkloadContainer, WorkloadRef } from '../tenant';
import { RESULT_CODES, StepTable } from './ApplicationDeploymentPlan';
import { diffWorkload, parseWorkloadConfiguration, toWorkloadSpec, tracksTag, workloadUnsupportedLabel, type WorkloadSpec } from './workloadConfiguration';
import { Group, int, Lines, Num, Text } from './configurationGroups/fields';
import { EnvRows } from './configurationGroups/environment';
import { displayName } from './Endpoints';
import { NOT_FOUND, NOT_GRANTED, RUN_NOT_FOUND, UPGRADE_CLUSTER, WORKLOAD_STEPS, workloadRefusal } from './workloadTexts';

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
const MAX_CONTAINERS = 16;
const BLANK: WorkloadContainer = { name: '', image: '', image_id: '', command: [], args: [], env: [], resources: { cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' } };
// A run creates a Deployment; the server refuses a create with paused, init containers or imports.
const NEW_SPEC: WorkloadSpec = { resource_version: '', replicas: 1, paused: false, strategy: 'RollingUpdate', containers: [BLANK], init_containers: [], env_from: [], unsupported: [] };

// onReread asks the owner for a fresh read; conflict is a settled apply refused as stale.
type FormProps = { base: string; onSent: (command: DirectCommand, target: WorkloadRef) => void; pending?: boolean; onStarted?: () => void; onReread?: () => void; conflict?: boolean }
  & ({ mode?: 'edit'; initial: WorkloadConfiguration } | { mode: 'run'; namespaces: string[]; seed?: WorkloadConfiguration });

// WorkloadConfigurationForm edits replicas and each container's image, command, arguments,
// literal environment and resources. Init containers, strategy and references pass through as
// read. Save applies at the read's resource_version, so a workload changed since is refused.
// Run mode creates a Deployment in a granted namespace instead, and may add and remove containers.
export function WorkloadConfigurationForm(props: FormProps) {
  const { base, onSent, pending = false, onReread, conflict = false, onStarted } = props;
  const initial = props.mode === 'run' ? null : props.initial;
  const namespaces = props.mode === 'run' ? props.namespaces : [];
  const run = !initial;
  const seed = props.mode === 'run' ? props.seed : undefined;
  const start = build(initial ? toWorkloadSpec(initial) : seed ? toWorkloadSpec(seed) : NEW_SPEC);
  const [draft, setDraft] = useState<WorkloadSpec>(start);
  const [namespace, setNamespace] = useState(seed?.target.namespace ?? (namespaces.length === 1 ? namespaces[0] ?? '' : ''));
  const [name, setName] = useState(seed?.target.name ?? '');
  const [confirm, setConfirm] = useState('');
  // Ticked pins: container names when editing, row indexes (as strings) in run mode.
  const [pins, setPins] = useState<ReadonlySet<string>>(new Set());
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [lost, setLost] = useState(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const target: WorkloadRef = initial ? initial.target : { namespace, kind: 'deployment', name };
  const spec = build(draft);
  const pull = spec.containers.filter((c, i) => pins.has(run ? String(i) : c.name) && tracksTag(c.image)).map((c) => c.name);
  const changes = [...diffWorkload(start, spec), ...(run ? [] : pull.map((n) => `containers.${n}.pin`))];
  const badReplicas = spec.replicas !== undefined && !(Number.isInteger(spec.replicas) && spec.replicas >= 0 && spec.replicas <= 1000);
  const names = spec.containers.map((c) => c.name);
  const incomplete = new Set(names).size !== names.length || spec.containers.some((c) => !dnsLabel.test(c.name) || c.image.trim() === '' || c.env.some((e) => e.name === ''));
  const badName = run && !dnsLabel.test(name);
  const ready = !badReplicas && !incomplete && spec.unsupported.length === 0 && (run ? namespaces.includes(namespace) && !badName : changes.length > 0 || pull.length > 0) && confirm === target.name;
  const togglePin = (key: string, on: boolean) => setPins((p) => { const n = new Set(p); if (on) n.add(key); else n.delete(key); return n; });
  const setContainer = (i: number, patch: Partial<WorkloadContainer>) => {
    setDraft((d) => ({ ...d, containers: d.containers.map((c, j) => j === i ? { ...c, ...patch } : c) }));
    if (!run && patch.image !== undefined) {
      const cname = draft.containers[i]?.name ?? '';
      togglePin(cname, patch.image !== start.containers.find((c) => c.name === cname)?.image);
    }
  };
  // Removing a run-mode row drops its pin and shifts the later ones down with their rows.
  const removeContainer = (i: number) => {
    setDraft((d) => ({ ...d, containers: d.containers.filter((_, j) => j !== i) }));
    setPins((p) => new Set([...p].map(Number).filter((k) => k !== i).map((k) => String(k > i ? k - 1 : k))));
  };
  const submit = async () => {
    onStarted?.();
    setBusy(true); setError('');
    const [url, body] = run ? [`${base}/workloads`, { spec: { target, ...spec, managed: false }, confirm, pull }] : [`${workloadURL(base, target)}/apply`, { resource_version: start.resource_version, spec, confirm, pull }];
    try {
      const resp = await secureFetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!alive.current) return;
      if (!resp.ok) { const text = await workloadRefusal(resp, run ? RUN_NOT_FOUND : NOT_FOUND); if (alive.current) setError(text); return; }
      const cmd: DirectCommand = await resp.json();
      if (!alive.current) return;
      setConfirm(''); onSent(cmd, target);
    } catch {
      if (alive.current) { setLost(true); setError(run ? "Connection lost. The run may have been sent. Check the cluster's Activity tab before running it again." : "Connection lost. The change may have been sent. Check the workload's Activity tab and read the workload again before retrying."); }
    } finally { if (alive.current) setBusy(false); }
  };
  return <section className="ky-config-form" aria-label={run ? 'Run a workload' : 'Edit workload'}>
    {spec.unsupported.length > 0 && <div className="dr-alert dr-alert-warn">
      <p>This workload has settings KyYard cannot carry over, so saving is disabled. The workload:</p>
      <ul className="ky-list">{spec.unsupported.map((c) => <li key={c}>{workloadUnsupportedLabel(c)}</li>)}</ul>
    </div>}
    <fieldset className="ky-config-groups" disabled={busy || pending}>
      {run && <Group title="Deployment">
        <label>Namespace<select aria-label="Namespace" value={namespace} onChange={(e) => setNamespace(e.target.value)}>
          <option value="">Choose a namespace</option>
          {namespaces.map((ns) => <option key={ns} value={ns}>{displayName(ns)}</option>)}
        </select></label>
        <Text label="Workload name" value={name} onChange={(next) => { setName(next); setDraft((d) => ({ ...d, containers: d.containers.map((c, i) => i === 0 && (c.name === '' || c.name === name) ? { ...c, name: next } : c) })); }} />
      </Group>}
      {start.replicas !== undefined && <details className="ky-config-disclosure"><summary>Scale · {Number.isFinite(draft.replicas) ? draft.replicas : 'invalid'} replicas</summary><Group title="Scale"><Num label="Replicas" inputMode="numeric" value={start.replicas} onChange={(v) => setDraft((d) => ({ ...d, replicas: v.trim() === '' ? NaN : int(v) }))} /></Group></details>}
      {initial && (initial.env_from.length > 0 || initial.init_containers.length > 0) && <Group title="Imported">
        {initial.env_from.length > 0 && <p>Imported from: {initial.env_from.map(displayName).join(', ')}</p>}
        {initial.init_containers.length > 0 && <p>Init containers: {initial.init_containers.length}, kept as read.</p>}
      </Group>}
      {draft.containers.map((c, i) => {
        // A run names containers by position; a count change remounts them, hiding revealed values.
        const who = run ? `container ${i + 1}` : c.name;
        return <Group key={run ? `${i}/${draft.containers.length}` : c.name} title={run ? `Container ${i + 1}` : `Container ${c.name}`}>
          {run && i > 0 && <Text label={`Name of ${who}`} value={c.name} onChange={(n) => setContainer(i, { name: n })} />}
          <Text label={`Image of ${who}`} value={c.image} onChange={(image) => setContainer(i, { image })} />
          {!run && c.image_id && <p>Running <code style={{ overflowWrap: 'anywhere' }}>{c.image_id}</code></p>}
          {tracksTag(c.image) && <label><input type="checkbox" aria-label={`Pin the current digest of ${who}`} checked={pins.has(run ? String(i) : c.name)} onChange={(e) => togglePin(run ? String(i) : c.name, e.target.checked)} /> Pin the reference's current digest</label>}
          <details className="ky-config-disclosure"><summary>Container settings</summary>
          {run && i === 0 && <Text label={`Name of ${who}`} value={c.name} onChange={(n) => setContainer(i, { name: n })} />}
          <Lines label={`Command of ${who}`} value={c.command} onChange={(command) => setContainer(i, { command })} />
          <Lines label={`Arguments of ${who}`} value={c.args} onChange={(args) => setContainer(i, { args })} />
          <Group title={`Environment of ${who}`}><EnvRows noun={`${run ? `Container ${i + 1}` : c.name} variable`} env={c.env} blank={{ name: '', value: '' }} onChange={(env) => setContainer(i, { env })} /></Group>
          {RESOURCES.map(([key, label]) => <Text key={key} label={`${label} of ${who}`} placeholder="unset" value={c.resources[key]} onChange={(v) => setContainer(i, { resources: { ...c.resources, [key]: v } })} />)}
          </details>
          {run && draft.containers.length > 1 && <div><button type="button" className="btn-secondary" onClick={() => removeContainer(i)}>Remove container {i + 1}</button></div>}
        </Group>;
      })}
      {run && draft.containers.length < MAX_CONTAINERS && <div><button type="button" className="btn-secondary" onClick={() => setDraft((d) => ({ ...d, containers: [...d.containers, BLANK] }))}>Add a container</button></div>}
    </fieldset>
    <div className="ky-config-save">
      {run ? <p>Running creates a Deployment in the chosen namespace, and Kubernetes starts its pods.</p> : <>
        <p>Saving updates this {KIND_NAMES[target.kind] ?? 'workload'} in place and Kubernetes replaces its pods. Nothing rolls back. A ticked image is resolved at the registry and pinned by digest; the tag stays visible.</p>
        <p>{changes.length ? `Changes: ${changes.join(', ')}` : 'No changes.'}</p>
      </>}
      {badName && name !== '' && <p>Enter the workload name as a lower-case DNS label: letters, digits and hyphens, at most 63 characters.</p>}
      {badReplicas && <p>Enter replicas as a whole number from 0 to 1000.</p>}
      {incomplete && (name !== '' || draft.containers.some((c) => c.image !== '')) && <p>Every container needs a unique lower-case name and an image, and every variable a name.</p>}
      <label>{run ? 'Type the new workload name to confirm' : `Type the workload name ${target.name} to confirm`}<input value={confirm} autoComplete="off" onChange={(e) => setConfirm(e.target.value)} /></label>
      <div><button type="button" disabled={!ready || busy || pending || lost} onClick={() => void submit()}>{run ? 'Run workload' : 'Save and apply'}</button>
        {(lost || conflict) && onReread && <button type="button" className="btn-secondary" onClick={onReread}>Read again</button>}</div>
    </div>
    {error && <p role="alert" className="dr-alert dr-alert-error">{error}</p>}
  </section>;
}

const OUTCOMES: Record<string, string> = {
  succeeded: 'Applied.',
  failed: 'The cluster did not complete the change; the steps say which. Nothing was rolled back.',
  denied: 'The cluster refused the change; nothing was applied.',
  timed_out: "The cluster agent did not answer in time; check the cluster's Activity tab before trying again.",
  unknown: 'The outcome is unknown: the agent may or may not have applied it. Read the workload again before retrying.',
};
// WorkloadResult is a settled apply or run: outcome, result code and the step table in workload words.
export function WorkloadResult({ command }: { command: DirectCommand }) {
  const outcome = command.action === 'workload.run' && command.outcome === 'succeeded' ? 'Running.'
    : Object.hasOwn(OUTCOMES, command.outcome) ? OUTCOMES[command.outcome] : 'Unrecognised outcome.';
  return <div role="status">
    <p>{outcome}</p>
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
