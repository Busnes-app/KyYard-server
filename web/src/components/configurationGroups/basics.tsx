import type { ExplicitSpec } from '../../tenant';
import { Check, Group, int, Lines, num, Num, Text, type GroupProps } from './fields';

const MiB = 1048576;
// Swap and PIDs use -1 for unlimited; it stays -1 through the MiB conversion.
const toMiB = (bytes: number) => bytes < 0 ? bytes : bytes / MiB;
const toBytes = (mib: number) => mib < 0 ? -1 : Math.round(mib * MiB);

// Changing the reference drops the kept digest and image ID: both belong to the reference they
// were read with. The server keeps the local image whenever an image ID is present.
export function ImageGroup({ spec, set, initial, run }: GroupProps & { initial: ExplicitSpec; run: boolean }) {
  const kept = initial.image.digest;
  const sameReference = spec.image.reference === initial.image.reference;
  return <Group title="Image">
    <Text label="Container name" value={spec.name} onChange={(name) => set({ name })} />
    <Text label="Image reference" value={spec.image.reference} placeholder="nginx:1.27" onChange={(reference) => { const same = reference === initial.image.reference; set({ image: { ...spec.image, reference, digest: same ? spec.image.digest : '' }, image_id: same ? spec.image_id : '' }); }} />
    {!run && <>
      {kept && <p>Digest <code style={{ overflowWrap: 'anywhere' }}>{kept}</code></p>}
      <Check label="Pull the reference's current digest" checked={spec.image_id === ''} onChange={(pull) => { const keep = !pull && sameReference; set({ image: { ...spec.image, digest: keep ? kept : '' }, image_id: keep ? initial.image_id : '' }); }} />
      {spec.image_id !== '' && <p>The host's image is kept; tick to pull the reference's current digest instead.</p>}
    </>}
  </Group>;
}

export function CommandGroup({ spec, set }: GroupProps) {
  return <Group title="Command">
    <Lines label="Entrypoint, one argument per line" value={spec.entrypoint} onChange={(entrypoint) => set({ entrypoint })} />
    <Lines label="Command, one argument per line" value={spec.command} onChange={(command) => set({ command })} />
    <Text label="Working directory" value={spec.working_dir} onChange={(working_dir) => set({ working_dir })} />
    <Text label="User" value={spec.user} onChange={(user) => set({ user })} />
    <Text label="Hostname" value={spec.hostname} onChange={(hostname) => set({ hostname })} />
  </Group>;
}

export function RestartGroup({ spec, set }: GroupProps) {
  return <Group title="Restart">
    <label>Restart policy<select value={spec.restart} onChange={(e) => set({ restart: e.target.value })}>
      {['no', 'always', 'unless-stopped', 'on-failure'].map((r) => <option key={r} value={r}>{r}</option>)}
    </select></label>
    {spec.restart === 'on-failure' && <Num label="Maximum retries" value={spec.restart_retries} onChange={(v) => set({ restart_retries: int(v) })} />}
  </Group>;
}

export function ResourcesGroup({ spec, set }: GroupProps) {
  const r = spec.resources;
  return <Group title="Resources">
    <p>0 means no limit; -1 swap or PIDs means unlimited.</p>
    <Num label="CPUs" value={r.nano_cpus / 1e9} onChange={(v) => set({ resources: { ...r, nano_cpus: Math.round(num(v) * 1e9) } })} />
    <Num label="Memory (MiB)" value={toMiB(r.memory_bytes)} onChange={(v) => set({ resources: { ...r, memory_bytes: toBytes(num(v)) } })} />
    <Num label="Memory and swap (MiB)" value={toMiB(r.memory_swap_bytes)} onChange={(v) => set({ resources: { ...r, memory_swap_bytes: toBytes(num(v)) } })} />
    <Num label="PIDs limit" value={r.pids_limit} onChange={(v) => set({ resources: { ...r, pids_limit: int(v) } })} />
  </Group>;
}

const noCheck = { test: [], interval_seconds: 0, timeout_seconds: 0, start_period_seconds: 0, retries: 0 };

export function HealthGroup({ spec, set }: GroupProps) {
  const h = spec.healthcheck;
  const mode = h === null ? 'image' : h.test[0] === 'NONE' ? 'disabled' : 'command';
  return <Group title="Health check">
    <label>Health check<select value={mode} onChange={(e) => set({ healthcheck: e.target.value === 'image' ? null : e.target.value === 'disabled' ? { ...noCheck, test: ['NONE'] } : { ...noCheck, test: ['CMD-SHELL', ''] } })}>
      <option value="image">Image default</option><option value="disabled">Disabled</option><option value="command">Command</option>
    </select></label>
    {mode === 'disabled' && <p>Health check disabled.</p>}
    {h && mode === 'command' && <>
      <Lines label="Test, one argument per line (CMD or CMD-SHELL first)" value={h.test} onChange={(test) => set({ healthcheck: { ...h, test } })} />
      <Num label="Interval (seconds)" value={h.interval_seconds} onChange={(v) => set({ healthcheck: { ...h, interval_seconds: num(v) } })} />
      <Num label="Timeout (seconds)" value={h.timeout_seconds} onChange={(v) => set({ healthcheck: { ...h, timeout_seconds: num(v) } })} />
      <Num label="Start period (seconds)" value={h.start_period_seconds} onChange={(v) => set({ healthcheck: { ...h, start_period_seconds: num(v) } })} />
      <Num label="Retries" value={h.retries} onChange={(v) => set({ healthcheck: { ...h, retries: int(v) } })} />
    </>}
  </Group>;
}

export function MiscGroup({ spec, set }: GroupProps) {
  return <Group title="Other settings">
    <Lines label="DNS servers, one per line" value={spec.dns} onChange={(dns) => set({ dns })} />
    <Lines label="Extra hosts (name:address), one per line" value={spec.extra_hosts} onChange={(extra_hosts) => set({ extra_hosts })} />
    <Text label="Stop signal" value={spec.stop_signal} placeholder="SIGTERM" onChange={(stop_signal) => set({ stop_signal })} />
    <Num label="Stop timeout (seconds, empty for the default)" value={spec.stop_timeout ?? ''} onChange={(v) => set({ stop_timeout: v.trim() === '' ? undefined : int(v) })} />
    <Check label="Allocate a TTY" checked={spec.tty} onChange={(tty) => set({ tty })} />
    <Check label="Keep stdin open" checked={spec.stdin_open} onChange={(stdin_open) => set({ stdin_open })} />
  </Group>;
}
