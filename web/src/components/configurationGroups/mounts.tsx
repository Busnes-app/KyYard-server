import type { ConfigMount } from '../../tenant';
import { needsAck } from '../containerConfiguration';
import { Check, Group, int, Rows, Text, type GroupProps } from './fields';

export function PortsGroup({ spec, set }: GroupProps) {
  return <Group title="Ports">
    <p>Leave the host port empty to expose the port without publishing it.</p>
    <Rows noun="Port" rows={spec.ports} blank={{ host: 0, container: 0, protocol: 'tcp' }} onChange={(ports) => set({ ports })} render={(p, update, label) => <>
      <input aria-label={`${label} container port`} placeholder="container port" type="number" value={p.container || ''} onChange={(e) => update({ ...p, container: int(e.target.value) })} />
      <input aria-label={`${label} host IP`} placeholder="host IP" value={p.host_ip ?? ''} onChange={(e) => update({ ...p, host_ip: e.target.value })} />
      <input aria-label={`${label} host port`} placeholder="host port" type="number" value={p.host || ''} onChange={(e) => update({ ...p, host: int(e.target.value) })} />
      <select aria-label={`${label} protocol`} value={p.protocol} onChange={(e) => update({ ...p, protocol: e.target.value })}><option value="tcp">tcp</option><option value="udp">udp</option></select>
    </>} />
  </Group>;
}

// A bind the container did not already grant needs the operator's acknowledgement of the host path.
export function VolumesGroup({ spec, set, known, acks, onAck }: GroupProps & { known: ConfigMount[]; acks: ReadonlySet<string>; onAck: (source: string, ok: boolean) => void }) {
  return <Group title="Volumes">
    <Rows noun="Mount" rows={spec.mounts} blank={{ kind: 'volume', source: '', target: '', read_only: false } as ConfigMount} onChange={(mounts) => set({ mounts })} render={(m, update, label) => <>
      <select aria-label={`${label} kind`} value={m.kind} onChange={(e) => update({ ...m, kind: e.target.value as ConfigMount['kind'] })}><option value="volume">volume</option><option value="bind">bind</option><option value="tmpfs">tmpfs</option></select>
      {m.kind !== 'tmpfs' && <input aria-label={`${label} source`} placeholder={m.kind === 'bind' ? '/host/path' : 'volume name'} value={m.source} onChange={(e) => update({ ...m, source: e.target.value })} />}
      <input aria-label={`${label} target`} placeholder="/path/in/container" value={m.target} onChange={(e) => update({ ...m, target: e.target.value })} />
      <Check label={`${label} read-only`} checked={m.read_only} onChange={(read_only) => update({ ...m, read_only })} />
      {m.source !== '' && needsAck(known, m) && <Check label={`This container will see host path ${m.source}`} checked={acks.has(m.source)} onChange={(ok) => onAck(m.source, ok)} />}
    </>} />
  </Group>;
}

export function NetworkGroup({ spec, set }: GroupProps) {
  return <Group title="Network">
    <Text label="Network mode" value={spec.network_mode} placeholder="bridge" onChange={(network_mode) => set({ network_mode })} />
    <Rows noun="Network" rows={spec.networks} blank={{ name: '', aliases: [], ip: '' }} onChange={(networks) => set({ networks })} render={(n, update, label) => <>
      <input aria-label={`${label} name`} value={n.name} onChange={(e) => update({ ...n, name: e.target.value })} />
      <input aria-label={`${label} aliases, comma separated`} value={n.aliases.join(',')} onChange={(e) => update({ ...n, aliases: e.target.value === '' ? [] : e.target.value.split(',') })} />
      <input aria-label={`${label} static IP`} placeholder="automatic" value={n.ip} onChange={(e) => update({ ...n, ip: e.target.value })} />
    </>} />
  </Group>;
}
