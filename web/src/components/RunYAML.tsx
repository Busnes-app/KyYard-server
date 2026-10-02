import { useState } from 'react';
import { secureFetch } from '../api';
import type { DirectCommand, ExplicitSpec, WorkloadConfiguration, WorkloadRef } from '../tenant';
import { ContainerConfigurationForm, CommandResult } from './ContainerConfigurationForm';
import { WorkloadConfigurationForm, WorkloadResult } from './WorkloadConfigurationForm';
import { parseConfiguration, toSpec } from './containerConfiguration';
import { parseWorkloadConfiguration, toWorkloadSpec } from './workloadConfiguration';
import { useCommand } from './ContainerControls';
import { Link } from './Link';
import { workloadRefusal } from './workloadTexts';
import { workloadPath } from '../router';
const object = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value);
type RunWorkload = WorkloadConfiguration & { run_manifest?: Record<string, unknown> };
interface Preview { containers: ExplicitSpec[]; workloads: RunWorkload[] }
export function parseRunPreview(value: unknown): Preview | null {
  if (!object(value) || !Array.isArray(value.containers) || !Array.isArray(value.workloads) || value.containers.length + value.workloads.length < 1 || value.containers.length + value.workloads.length > 16) return null;
  const containers: ExplicitSpec[] = [], workloads: RunWorkload[] = [];
  for (const item of value.containers) {
    if (!object(item)) return null;
    const target = { container_id: '', image_id: '', created_unix: 0 };
    const parsed = parseConfiguration({ ...item, target, observed_at: new Date().toISOString() }, target);
    if (!parsed || parsed.image_id !== '' || parsed.unsupported.length) return null;
    containers.push(toSpec(parsed));
  }
  for (const item of value.workloads) {
    if (!object(item) || !object(item.target) || typeof item.target.namespace !== 'string' || typeof item.target.name !== 'string' || item.target.kind !== 'deployment') return null;
    const target: WorkloadRef = { namespace: item.target.namespace, name: item.target.name, kind: item.target.kind };
    const parsed = parseWorkloadConfiguration(item, target);
    if (!parsed || parsed.resource_version !== '' || parsed.managed || parsed.unsupported.length) return null;
    if (item.run_manifest !== undefined) {
      if (!object(item.run_manifest) || item.run_manifest.apiVersion !== 'apps/v1' || item.run_manifest.kind !== 'Deployment' || !object(item.run_manifest.metadata) || item.run_manifest.metadata.name !== target.name || item.run_manifest.metadata.namespace !== target.namespace) return null;
      workloads.push({ ...parsed, run_manifest: item.run_manifest });
    } else workloads.push(parsed);
  }
  return { containers, workloads };
}
export function RunYAML({ base, runtime, namespaces = [], org, endpoint, onStarted }: { onStarted?: () => void; base: string; runtime: 'docker' | 'kubernetes'; namespaces?: string[]; org: string; endpoint: string }) {
  const [source, setSource] = useState('');
  const [preview, setPreview] = useState<Preview | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function review() {
    setBusy(true); setError('');
    try {
      const response = await secureFetch(`${base}/run-yaml/preview`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ yaml: source }) });
      const body: unknown = await response.json();
      if (!response.ok) { const line = object(body) && object(body.diagnostic) && typeof body.diagnostic.line === 'number' && Number.isInteger(body.diagnostic.line) && body.diagnostic.line > 0 ? ` near line ${body.diagnostic.line}` : ''; setError(`YAML could not be reviewed${line}. Check the supported fields, document size and namespace grants. Nothing was run.`); return; }
      const parsed = parseRunPreview(body);
      if (!parsed || (runtime === 'docker' ? parsed.workloads.length > 0 : parsed.containers.length > 0)) { setError('The preview did not match this endpoint. Nothing was run.'); return; }
      setPreview(parsed); setSource('');
    } catch { setError('The preview connection failed. Nothing was run.'); }
    finally { setBusy(false); }
  }
  return <section aria-label="Run YAML">
    <p>Paste {runtime === 'docker' ? 'Docker Compose' : 'Kubernetes Deployment'} YAML, review each resource, then confirm its name to run it.</p>
    <details><summary>Supported YAML fields</summary><p>{runtime === 'docker' ? 'Up to 16 services: image, container_name, argv-list command/entrypoint, explicit string environment, restart, quoted short or long-form ports and bind or existing external volumes. Build, networks and interpolation are refused.' : 'Up to 16 apps/v1 Deployments: metadata name and namespace, replicas, strategy type, template containers with name, image, command, args, literal environment and CPU/memory requests or limits. Native Deployment selectors, ports, probes, security settings and existing volume references are preserved. Service-account token mounts are disabled; custom service accounts and KyYard ownership labels are refused. Other resource kinds, including Services and PVC creation, use the managed application path.'} Maximum 64 KiB. Unsupported fields stop the entire preview.</p></details>
    {!preview && <><label>YAML<textarea aria-label="YAML" rows={16} value={source} autoComplete="off" spellCheck={false} onChange={(event) => setSource(event.target.value)} /></label><button disabled={busy || source.length === 0 || new TextEncoder().encode(source).length > 65536} onClick={() => void review()}>{busy ? 'Reviewing…' : 'Review YAML'}</button></>}
    {error && <p role="alert">{error}</p>}
    {preview && <><p>Preview ready. Nothing has run. Each resource runs independently; a failure leaves successful resources in place.</p>{preview.containers.map((seed, i) => <YAMLContainer key={i} onStarted={onStarted} base={base} seed={seed} org={org} endpoint={endpoint} />)}{preview.workloads.map((seed, i) => <YAMLWorkload key={i} onStarted={onStarted} base={base} seed={seed} namespaces={namespaces} org={org} endpoint={endpoint} />)}</>}
  </section>;
}
function YAMLContainer({ base, seed, org, endpoint, onStarted }: { onStarted?: () => void; base: string; seed: ExplicitSpec; org: string; endpoint: string }) {
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const { command, error } = useCommand(base, sent);
  return <section className="panel"><h2>{seed.name}</h2><ContainerConfigurationForm base={base} mode="run" seed={seed} pending={!!sent} onSent={setSent} onStarted={onStarted} />{command?.outcome ? <CommandResult command={command} org={org} endpoint={endpoint} current="" /> : sent && <p role="status">{error ? 'Result unavailable; check endpoint Activity before retrying.' : 'Waiting for the host. Do not retry.'}</p>}</section>;
}
function YAMLWorkload({ base, seed, namespaces, org, endpoint, onStarted }: { onStarted?: () => void; base: string; seed: RunWorkload; namespaces: string[]; org: string; endpoint: string }) {
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const { command, error } = useCommand(base, sent);
  return <section className="panel"><h2>{seed.target.namespace}/{seed.target.name}</h2>{seed.run_manifest ? <ManifestConfirmation base={base} seed={seed} pending={!!sent} onSent={setSent} onStarted={onStarted} /> : <WorkloadConfigurationForm base={base} mode="run" seed={seed} namespaces={namespaces} pending={!!sent} onSent={setSent} onStarted={onStarted} />}{command?.outcome ? <><WorkloadResult command={command} />{command.outcome === 'succeeded' && <Link to={workloadPath(org, endpoint, seed.target.namespace, 'deployment', seed.target.name)}>Open workload</Link>}</> : sent && <p role="status">{error ? 'Result unavailable; check endpoint Activity before retrying.' : 'Waiting for the cluster. Do not retry.'}</p>}</section>;
}

function ManifestConfirmation({ base, seed, pending, onSent, onStarted }: { onStarted?: () => void; base: string; seed: RunWorkload; pending: boolean; onSent: (command: DirectCommand) => void }) {
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [error, setError] = useState('');
  async function run() {
    onStarted?.();
    setBusy(true); setError('');
    try {
      const response = await secureFetch(`${base}/workloads`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ spec: { ...toWorkloadSpec(seed), target: seed.target, run_manifest: seed.run_manifest, managed: false }, confirm }) });
      if (!response.ok) { setError(await workloadRefusal(response)); if (response.status >= 500) setBlocked(true); return; }
      onSent(await response.json()); setConfirm('');
    } catch { setBlocked(true); setError('The outcome is unknown. Check cluster Activity before retrying.'); }
    finally { setBusy(false); }
  }
  return <><NativeManifestFacts manifest={seed.run_manifest} /><p>{seed.replicas} replicas · {seed.containers.map((c) => `${c.name}: ${c.image}`).join(', ')}</p><p>The native Deployment template is preserved. Service-account token mounting is off. Existing volume and Secret references stay in the selected namespace.</p><label>Type the workload name {seed.target.name} to confirm<input autoComplete="off" value={confirm} onChange={(event) => setConfirm(event.target.value)} disabled={pending || busy || blocked} /></label><button disabled={confirm !== seed.target.name || pending || busy || blocked} onClick={() => void run()}>Run YAML Deployment</button>{error && <p role="alert">{error}</p>}</>;
}

function NativeManifestFacts({ manifest }: { manifest: Record<string, unknown> | undefined }) {
  const spec = manifest && object(manifest.spec) ? manifest.spec : null;
  const template = spec && object(spec.template) ? spec.template : null;
  const pod = template && object(template.spec) ? template.spec : null;
  const containers = pod && Array.isArray(pod.containers) ? pod.containers : [];
  const volumes = pod && Array.isArray(pod.volumes) ? pod.volumes : [];
  return <details><summary>Review ports, environment & storage</summary><ul className="ky-list">{containers.filter(object).map((c, i) => <li key={i}>{typeof c.name === 'string' ? c.name : 'Container'}: ports {Array.isArray(c.ports) ? c.ports.filter(object).map((p) => typeof p.containerPort === 'number' ? p.containerPort : '').filter(Boolean).join(', ') || 'none' : 'none'}; environment keys {Array.isArray(c.env) ? c.env.filter(object).map((e) => typeof e.name === 'string' ? e.name : '').filter(Boolean).join(', ') || 'none' : 'none'}; mounts {Array.isArray(c.volumeMounts) ? c.volumeMounts.filter(object).map((m) => typeof m.mountPath === 'string' ? m.mountPath : '').filter(Boolean).join(', ') || 'none' : 'none'}</li>)}{volumes.filter(object).map((v, i) => <li key={`volume-${i}`}>Volume {typeof v.name === 'string' ? v.name : ''}{object(v.persistentVolumeClaim) && typeof v.persistentVolumeClaim.claimName === 'string' ? `: existing claim ${v.persistentVolumeClaim.claimName}` : ''}</li>)}</ul></details>;
}
