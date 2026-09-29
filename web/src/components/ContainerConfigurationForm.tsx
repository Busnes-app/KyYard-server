import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
import { refusal, type Container, type ContainerConfiguration, type DirectCommand, type ExplicitSpec, type WriteTexts } from '../tenant';
import { Link } from './Link';
import { containerPath } from '../router';
import { useCommand } from './ContainerControls';
import { RESULT_CODES, StepTable } from './ApplicationDeploymentPlan';
import { diff, toSpec, unsupportedLabel } from './containerConfiguration';
import { CommandGroup, HealthGroup, ImageGroup, MiscGroup, ResourcesGroup, RestartGroup } from './configurationGroups/basics';
import { EnvironmentGroup } from './configurationGroups/environment';
import { NetworkGroup, PortsGroup, VolumesGroup } from './configurationGroups/mounts';
import { LoggingGroup, SecurityGroup } from './configurationGroups/security';

type Props = {
  base: string; org: string; endpoint: string; mode: 'edit' | 'run';
  initial?: ContainerConfiguration; container?: Container;
  managed?: { application: string; link: string };
  onSettled: (command: DirectCommand) => void;
};

const EMPTY_SPEC: ExplicitSpec = {
  name: '', image: { reference: '', digest: '' }, image_id: '', command: [], entrypoint: [], user: '', working_dir: '', hostname: '',
  env: [], labels: {}, restart: 'no', restart_retries: 0, ports: [], mounts: [], network_mode: '', networks: [],
  resources: { nano_cpus: 0, memory_bytes: 0, memory_swap_bytes: 0, pids_limit: 0 }, healthcheck: null,
  privileged: false, read_only_rootfs: false, init: false, tty: false, stdin_open: false,
  cap_add: [], cap_drop: [], security_opt: [], extra_hosts: [], dns: [], devices: [], log: { driver: '', options: {} }, stop_signal: '', unsupported: [],
};

const sortedMap = (entries: [string, string][]) => Object.fromEntries([...entries].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
const list = (items: string[]) => items.map((s) => s.trim()).filter(Boolean);
const argv = (items: string[]) => { const out = [...items]; while (out.length && out[out.length - 1] === '') out.pop(); return out; };

// buildSpec is the one canonical shape: the read and the draft both pass through it, so diff()
// compares like with like (key order included) and a form nobody touched has no changes.
function buildSpec(s: ExplicitSpec, logOptions: [string, string][]): ExplicitSpec {
  const spec: ExplicitSpec = {
    name: s.name.trim(), image: { reference: s.image.reference.trim(), digest: s.image.digest, ...(s.image.tag !== undefined ? { tag: s.image.tag } : {}) }, image_id: s.image_id,
    command: argv(s.command), entrypoint: argv(s.entrypoint), user: s.user, working_dir: s.working_dir, hostname: s.hostname,
    env: s.env.map((e) => ({ name: e.name, value: e.value })), labels: sortedMap(Object.entries(s.labels)),
    restart: s.restart, restart_retries: s.restart === 'on-failure' ? s.restart_retries : 0,
    ports: s.ports.map((p) => p.host_ip ? { host_ip: p.host_ip, host: p.host, container: p.container, protocol: p.protocol } : { host: p.host, container: p.container, protocol: p.protocol }),
    mounts: s.mounts.map((m) => ({ kind: m.kind, source: m.kind === 'tmpfs' ? '' : m.source, target: m.target, read_only: m.read_only })),
    network_mode: s.network_mode, networks: s.networks.map((n) => ({ name: n.name, aliases: list(n.aliases), ip: n.ip })),
    resources: { ...s.resources },
    healthcheck: s.healthcheck && { test: argv(s.healthcheck.test), interval_seconds: s.healthcheck.interval_seconds, timeout_seconds: s.healthcheck.timeout_seconds, start_period_seconds: s.healthcheck.start_period_seconds, retries: s.healthcheck.retries },
    privileged: s.privileged, read_only_rootfs: s.read_only_rootfs, init: s.init, tty: s.tty, stdin_open: s.stdin_open,
    cap_add: list(s.cap_add), cap_drop: list(s.cap_drop), security_opt: list(s.security_opt), extra_hosts: list(s.extra_hosts), dns: list(s.dns),
    devices: s.devices.map((d) => ({ host: d.host, container: d.container, permissions: d.permissions })),
    log: { driver: s.log.driver, options: sortedMap(logOptions.filter(([k]) => k !== '')) },
    stop_signal: s.stop_signal, unsupported: [...s.unsupported],
  };
  if (s.stop_timeout !== undefined) spec.stop_timeout = s.stop_timeout;
  return spec;
}

const WRITE_TEXTS: WriteTexts = {
  forbidden: 'Only an organization administrator can change container configuration.',
  invalid: 'The server refused the request. Check the typed name and refresh the inventory.',
  notFound: 'The container or host is no longer in this access scope.',
  conflict: {
    command_in_progress: 'A recreate or run is already in progress on this host.',
    application_managed: 'An application manages this container. Change it through the application.',
    endpoint_offline: 'The host is not connected. Reconnect it before saving.',
    deployment_not_sent: 'The command was not sent to the host. Refresh and try again.',
    adoption_changed: 'The container changed since this form was loaded. Refresh before saving.',
    runtime_unsupported: 'Only a Docker host runs containers from this form.',
  },
};
const UPGRADE = 'Upgrade the host agent to enable editing.';
const BLOCKERS: Record<string, string> = {
  configuration_incomplete: 'The configuration read was incomplete, so this container cannot be recreated from it.',
  name_taken: 'Another container already uses that name.',
  port_conflict: 'Another container already publishes one of these host ports.',
  image_unresolved: "The image is no longer on the host. Tick \"Pull the reference's current digest\".",
  bind_unacknowledged: 'Acknowledge every new host path.',
};
// spec_invalid:<field> names the setting the server's configuration rules refused.
const FIELDS: Record<string, string> = {
  name: 'container name', image: 'image', image_id: 'image', argv: 'command or entrypoint', user_working_dir_or_hostname: 'user, working directory or hostname',
  restart: 'restart', network_mode: 'network mode', networks: 'networks', stop: 'stop', list: 'list', resources: 'resources', env: 'environment',
  labels: 'labels', ports: 'ports', mounts: 'mounts', healthcheck: 'health check', devices: 'devices', log: 'logging', size: 'size',
  frame_too_large: 'size', frame_invalid: 'deployment', too_many_registry_hosts: 'registry',
};
function blockerText(code: string): string {
  if (Object.hasOwn(BLOCKERS, code)) return BLOCKERS[code] ?? '';
  const field = code.startsWith('spec_invalid:') ? code.slice('spec_invalid:'.length) : '';
  return Object.hasOwn(FIELDS, field) ? `The server refused the ${FIELDS[field]} setting.` : 'The server refused part of this configuration.';
}
async function failure(resp: Response): Promise<string> {
  if (resp.status === 501) return UPGRADE;
  if (resp.status === 429) return 'Too many configuration requests. Wait a minute and try again.';
  if (resp.status !== 422) return refusal(resp, WRITE_TEXTS);
  const body: unknown = await resp.json().catch(() => null);
  const blockers = body && typeof body === 'object' && 'blockers' in body && Array.isArray(body.blockers) ? body.blockers.filter((b): b is string => typeof b === 'string') : [];
  return blockers.length ? [...new Set(blockers.map(blockerText))].join(' ') : 'The server refused part of this configuration.';
}
const OUTCOMES: Record<string, string> = {
  succeeded: 'Done.',
  failed: 'The host did not complete the change; the steps say which.',
  denied: 'The host refused the change; nothing was replaced.',
  unknown: 'The outcome is unknown: the host may or may not have acted. Check the container before trying again.',
};

export function ContainerConfigurationForm({ base, org, endpoint, mode, initial, container, managed, onSettled }: Props) {
  const start = initial ? toSpec(initial) : EMPTY_SPEC;
  const [draft, setDraft] = useState<ExplicitSpec>(start);
  const [logOptions, setLogOptions] = useState<[string, string][]>(Object.entries(start.log.options));
  const [acks, setAcks] = useState<ReadonlySet<string>>(new Set());
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [lost, setLost] = useState(false);
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const { command, error: pollError } = useCommand(base, sent, onSettled);
  const run = mode === 'run';
  const before = buildSpec(start, Object.entries(start.log.options));
  const spec = buildSpec(draft, logOptions);
  const changes = diff(before, spec);
  const known = run ? [] : start.mounts.filter((m) => m.kind === 'bind').map((m) => m.source);
  const newBinds = [...new Set(spec.mounts.filter((m) => m.kind === 'bind' && !known.includes(m.source)).map((m) => m.source))];
  const expected = run ? spec.name : container?.name ?? '';
  const pending = command !== null && !command.outcome;
  // The inventory lags the result by up to a report interval, so the page stays and links instead.
  const created = command?.outcome ? command.result?.services[0]?.container_id ?? '' : '';
  const incomplete = spec.env.some((e) => e.name === '') || spec.ports.some((p) => p.container === 0) || spec.devices.some((d) => d.host === '' || d.container === '');
  const r = spec.resources, h = spec.healthcheck;
  const numbers = [r.nano_cpus, r.memory_bytes, r.memory_swap_bytes, r.pids_limit, spec.restart_retries, spec.stop_timeout ?? 0, ...(h ? [h.interval_seconds, h.timeout_seconds, h.start_period_seconds, h.retries] : [])];
  const malformed = numbers.some((n) => !Number.isFinite(n));
  const ready = !managed && !incomplete && !malformed && spec.unsupported.length === 0 && (run ? spec.name !== '' && spec.image.reference !== '' : changes.length > 0 && !!container)
    && expected !== '' && confirm === expected && newBinds.every((b) => b !== '' && acks.has(b));
  const set = (patch: Partial<ExplicitSpec>) => setDraft((d) => ({ ...d, ...patch }));
  const submit = async () => {
    setBusy(true); setError('');
    const body = { spec, acknowledge_binds: newBinds, confirm };
    const url = run || !container ? `${base}/containers` : `${base}/containers/${encodeURIComponent(container.id)}/recreate`;
    const expects = run || !container ? {} : { expects: { image_id: container.image_id, created_unix: Math.floor(Date.parse(container.created_at) / 1000), state: container.state } };
    try {
      const resp = await secureFetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...expects, ...body }) });
      if (!alive.current) return;
      if (!resp.ok) { setError(await failure(resp)); return; }
      const cmd: DirectCommand = await resp.json();
      if (!alive.current) return;
      setConfirm(''); setSent(cmd);
      if (cmd.outcome) onSettled(cmd);
    } catch {
      if (alive.current) { setLost(true); setError('Connection lost. The change may have been sent. Check recent activity and refresh before trying again.'); }
    } finally { if (alive.current) setBusy(false); }
  };
  const groupProps = { spec: draft, set };
  return <section className="ky-config-form" aria-label={run ? 'Run a container' : 'Edit configuration'}>
    {managed && <p className="dr-alert dr-alert-warn">Managed by application {managed.application}. <Link to={managed.link}>Edit it there.</Link></p>}
    {spec.unsupported.length > 0 && <div className="dr-alert dr-alert-warn">
      <p>This container has settings KyYard cannot carry over, so saving is disabled. The container:</p>
      <ul className="ky-list">{spec.unsupported.map((c) => <li key={c}>{unsupportedLabel(c)}</li>)}</ul>
    </div>}
    {(run || initial) && <fieldset className="ky-config-groups" disabled={!!managed || busy || pending}>
      <ImageGroup {...groupProps} initial={start} run={run} />
      <CommandGroup {...groupProps} />
      <EnvironmentGroup {...groupProps} />
      <PortsGroup {...groupProps} />
      <VolumesGroup {...groupProps} known={known} acks={acks} onAck={(source, ok) => setAcks((a) => { const next = new Set(a); if (ok) next.add(source); else next.delete(source); return next; })} />
      <NetworkGroup {...groupProps} />
      <RestartGroup {...groupProps} />
      <ResourcesGroup {...groupProps} />
      <HealthGroup {...groupProps} />
      <SecurityGroup {...groupProps} />
      <LoggingGroup {...groupProps} options={logOptions} onOptions={setLogOptions} />
      <MiscGroup {...groupProps} />
    </fieldset>}
    {!managed && <div className="ky-config-save">
      {run ? <p>Running creates and starts a new container on this host.</p> : <>
        <p>Saving replaces the container with a new one built from this configuration; its ID changes. If the new one does not start, the host tries to restore the previous one.</p>
        <p>{changes.length ? `Changes: ${changes.join(', ')}` : 'No changes.'}</p>
      </>}
      {incomplete && <p>Complete every row: variable names, container ports and device paths.</p>}
      {malformed && <p>Enter a number in every numeric field.</p>}
      <label>{run ? 'Type the new container name to confirm' : `Type the container name ${expected} to confirm`}<input value={confirm} autoComplete="off" onChange={(e) => setConfirm(e.target.value)} /></label>
      <div><button type="button" disabled={!ready || busy || pending || lost} onClick={() => void submit()}>{run ? 'Run container' : 'Save and recreate'}</button></div>
    </div>}
    {error && <p role="alert" className="dr-alert dr-alert-error">{error}</p>}
    {pending && <p role="status">Command sent; waiting for the host. Do not retry while its outcome is unknown.</p>}
    {pollError && <p role="alert" className="dr-alert dr-alert-error">Could not read the command result. Check recent activity before trying again.</p>}
    {command?.outcome && <div role="status">
      <p>{Object.hasOwn(OUTCOMES, command.outcome) ? OUTCOMES[command.outcome] : 'Unrecognised outcome.'}</p>
      {command.result?.code && Object.hasOwn(RESULT_CODES, command.result.code) && <p>{RESULT_CODES[command.result.code]}</p>}
      {command.result && command.result.steps.length > 0 && <StepTable steps={command.result.steps} />}
      {created && <p><Link to={containerPath(org, endpoint, created, 'configuration')}>Open the new container</Link></p>}
    </div>}
  </section>;
}
