import { useEffect, useRef, useState } from 'react';
import { ContainerPorts } from './ContainerPorts';
import type { Port } from '../tenant';

export type InspectionTarget = { container_id: string; image_id: string; created_unix: number };
// Human names of the agent's unsupported-configuration codes (protocol.UnsupportedCodes).
// A code missing here is refused, never shown raw.
export const unsupportedNames: Record<string, string> = {
  mount_type: 'has mounts other than volumes and binds',
  anonymous_volume: 'uses anonymous volumes',
  volumes_from: 'mounts volumes from another container',
  volume_driver: 'uses a volume driver',
  mount_options: 'sets mount options',
  tmpfs: 'mounts tmpfs',
  auto_remove: 'removes itself when stopped',
  read_only_rootfs: 'has a read-only root filesystem',
  privileged: 'runs privileged',
  capabilities: 'adds or drops capabilities',
  security_opt: 'sets security options',
  devices: 'maps host devices',
  pid_mode: 'shares a PID namespace',
  ipc_mode: 'shares an IPC namespace',
  user: 'runs as a set user',
  runtime: 'uses a non-default runtime',
  resource_limits: 'has memory, CPU or process limits',
  ulimits: 'sets ulimits',
  sysctls: 'sets sysctls',
  device_requests: 'requests GPUs or other devices',
  init: 'runs an init process',
  userns_mode: 'sets a user namespace mode',
  cgroup_parent: 'sets a cgroup parent',
  group_add: 'adds supplementary groups',
  extra_hosts: 'adds host entries',
  dns: 'sets DNS options',
  links: 'uses container links',
  network: 'is on a network other than its project network',
  image_config: "overrides its image's command, entrypoint, healthcheck, working directory or stop signal",
  k8s_volume: 'mounts a host path, or more than eight named volumes; a Deployment takes named volumes only, as claims',
  k8s_host_ip: 'publishes a port on a host address; a Kubernetes Service has none, so drop the address',
  k8s_restart: 'sets a restart policy other than always or unless-stopped; a Deployment always restarts',
  k8s_name: 'has a name Kubernetes cannot use as a label; end it with a letter or digit',
  k8s_namespace: "is mapped to a namespace the cluster's manifest no longer grants",
  k8s_volume_shared: 'mounts a named volume another service mounts too; a ReadWriteOnce claim serves one pod',
};
// K8S_VOLUME_CHOICE replaces k8s_volume's sentence when its detail is choice_required.
export const K8S_VOLUME_CHOICE = "mounts a named volume with no storage choice. Each revision keeps the previous one's choices; a newly declared volume has none, so remove it from the definition or migrate the application again";
type Inspection = {
  observed_at: string; state: string; image_platform: { os: string; architecture: string; variant?: string };
  restart_policy: string; restart_retries: number; ports: Port[];
  mounts: { bind: number; volume: number; tmpfs: number; other: number; read_only: number };
  network_mode: string; network_count: number; privileged: boolean; read_only_rootfs: boolean; auto_remove: boolean;
  configuration_verified: boolean; unsupported: string[];
};
type Result = { kind: 'loading' } | { kind: 'error'; message: string } | { kind: 'ready'; data: Inspection };
const count = (value: unknown): value is number => typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= 64;
const token = (value: unknown): value is string => typeof value === 'string' && /^[a-z0-9][a-z0-9_.-]{0,63}$/.test(value);
function object(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === 'object' && !Array.isArray(value); }
function parseInspection(value: unknown, target: InspectionTarget): Inspection | null {
  if (!object(value) || !object(value.target) || value.target.container_id !== target.container_id || value.target.image_id !== target.image_id || value.target.created_unix !== target.created_unix) return null;
  const { observed_at, state, image_platform, restart_policy, restart_retries, ports, mounts, network_mode, network_count, privileged, read_only_rootfs, auto_remove, configuration_verified, unsupported } = value;
  if (typeof observed_at !== 'string' || !Number.isFinite(Date.parse(observed_at)) || typeof state !== 'string' || !['created', 'running', 'paused', 'restarting', 'removing', 'exited', 'dead'].includes(state)) return null;
  if (!object(image_platform) || !token(image_platform.os) || !token(image_platform.architecture) || (image_platform.variant !== undefined && !token(image_platform.variant))) return null;
  if (typeof restart_policy !== 'string' || !['no', 'always', 'unless-stopped', 'on-failure'].includes(restart_policy) || typeof restart_retries !== 'number' || !Number.isInteger(restart_retries) || restart_retries < 0 || restart_retries > 2147483647) return null;
  if (typeof network_mode !== 'string' || !['default', 'bridge', 'host', 'none', 'container', 'custom'].includes(network_mode) || !count(network_count)) return null;
  if (typeof privileged !== 'boolean' || typeof read_only_rootfs !== 'boolean' || typeof auto_remove !== 'boolean') return null;
  if (typeof configuration_verified !== 'boolean' || !Array.isArray(unsupported) || unsupported.length > 32 || new Set(unsupported).size !== unsupported.length
    || !unsupported.every((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) || configuration_verified !== (unsupported.length === 0)) return null;
  if (!object(mounts) || !count(mounts.bind) || !count(mounts.volume) || !count(mounts.tmpfs) || !count(mounts.other) || !count(mounts.read_only)) return null;
  const total = mounts.bind + mounts.volume + mounts.tmpfs + mounts.other;
  if (total > 64 || mounts.read_only > total || !Array.isArray(ports) || ports.length > 64) return null;
  const safePorts: Port[] = [];
  for (const port of ports) {
    if (!object(port) || typeof port.container !== 'number' || !Number.isInteger(port.container) || port.container < 1 || port.container > 65535 || typeof port.protocol !== 'string' || !['tcp', 'udp', 'sctp'].includes(port.protocol)) return null;
    if (port.host !== undefined && (typeof port.host !== 'number' || !Number.isInteger(port.host) || port.host < 0 || port.host > 65535)) return null;
    if (port.host_ip !== undefined && (typeof port.host_ip !== 'string' || !/^[a-fA-F0-9:.]{0,45}$/.test(port.host_ip))) return null;
    safePorts.push({ container: port.container, protocol: port.protocol, host: port.host, host_ip: port.host_ip });
  }
  return { observed_at, state, image_platform: { os: image_platform.os, architecture: image_platform.architecture, variant: image_platform.variant }, restart_policy, restart_retries, ports: safePorts, mounts: { bind: mounts.bind, volume: mounts.volume, tmpfs: mounts.tmpfs, other: mounts.other, read_only: mounts.read_only }, network_mode, network_count, privileged, read_only_rootfs, auto_remove, configuration_verified, unsupported: [...unsupported] };
}
function failure(status: number): string {
  if (status === 401 || status === 403) return 'Inspection access was denied. Check your sign-in and host access.';
  if (status === 404 || status === 409) return 'The host or container changed, or inspection is unavailable. Close and refresh preflight before retrying.';
  if (status === 429) return 'Inspection capacity reached. Close and try again later.';
  if (status === 501) return 'Upgrade the host agent to enable live inspection.';
  if (status === 504) return 'Inspection timed out. Check the host connection before retrying.';
  return 'Inspection could not be completed. Check the host and refresh preflight.';
}

export function ApplicationInspection({ org, endpoint, service, target, onClose }: { org: string; endpoint: string; service: string; target: InspectionTarget; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [result, setResult] = useState<Result>({ kind: 'loading' });
  useEffect(() => { dialog.current?.showModal(); }, []);
  useEffect(() => {
    const controller = new AbortController();
    setResult({ kind: 'loading' });
    async function read() {
      try {
        const response = await fetch(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}/containers/${encodeURIComponent(target.container_id)}/inspection`, { signal: controller.signal, cache: 'no-store' });
        if (controller.signal.aborted) return;
        if (!response.ok) { setResult({ kind: 'error', message: failure(response.status) }); return; }
        const payload: unknown = await response.json();
        if (controller.signal.aborted) return;
        const data = parseInspection(payload, target);
        setResult(data ? { kind: 'ready', data } : { kind: 'error', message: 'Inspection did not match the preflight target or response format. Close and refresh preflight.' });
      } catch {
        if (!controller.signal.aborted) setResult({ kind: 'error', message: 'The inspection connection was lost. Check the host before retrying.' });
      }
    }
    void read();
    return () => controller.abort();
  }, [org, endpoint, target]);
  return <dialog ref={dialog} className="modal-window ky-log-dialog" aria-label={`Live inspection for ${service}`} onCancel={(event) => { event.preventDefault(); onClose(); }} onClose={onClose}>
    <div className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
      <h3>Live inspection: {service}</h3>
      <button type="button" className="btn-secondary" onClick={onClose} autoFocus>Close inspection</button>
      <p>Read-only observation of the mapped container. It says whether a recreate from the definition could keep the container's configuration; it is not a replacement specification and approves nothing.</p>
      <p>Container: <code>{target.container_id}</code><br />Container image: <code>{target.image_id}</code></p>
      {result.kind === 'loading' && <p role="status">Inspecting container…</p>}
      {result.kind === 'error' && <p role="alert">{result.message}</p>}
      {result.kind === 'ready' && <InspectionFacts data={result.data} />}
    </div>
  </dialog>;
}
function InspectionFacts({ data }: { data: Inspection }) {
  return <>
    <p>Observed {new Date(data.observed_at).toLocaleString()}. Snapshot only; close and reopen to inspect again.</p>
    {data.configuration_verified
      ? <p>Configuration: fully expressible</p>
      : <><p>Configuration the definition cannot express, which a recreate would drop:</p><ul className="ky-list">{data.unsupported.map(c => <li key={c}>{unsupportedNames[c]}</li>)}</ul></>}
    <dl>
      <dt>State</dt><dd>{data.state}</dd>
      <dt>Container image platform</dt><dd>{[data.image_platform.os, data.image_platform.architecture, data.image_platform.variant].filter(Boolean).join('/')}</dd>
      <dt>Restart policy</dt><dd>{data.restart_policy}{data.restart_policy === 'on-failure' && ` (${data.restart_retries === 0 ? 'unlimited retries' : `${data.restart_retries} retries`})`}</dd>
      <dt>Networking</dt><dd>{data.network_mode} · {data.network_count} attached networks</dd>
      <dt>Mounts</dt><dd>{data.mounts.bind} bind · {data.mounts.volume} volume · {data.mounts.tmpfs} tmpfs · {data.mounts.other} other · {data.mounts.read_only} read-only</dd>
      <dt>Privileged</dt><dd>{data.privileged ? 'Yes' : 'No'}</dd>
      <dt>Read-only root filesystem</dt><dd>{data.read_only_rootfs ? 'Yes' : 'No'}</dd>
      <dt>Automatic removal</dt><dd>{data.auto_remove ? 'Yes' : 'No'}</dd>
    </dl>
    <h4>Reported ports</h4><ContainerPorts ports={data.ports} />
    <p>Mount paths, network names, environment values and commands are omitted. The desired image may differ from this container image; platform and configuration compatibility are not established.</p>
  </>;
}
