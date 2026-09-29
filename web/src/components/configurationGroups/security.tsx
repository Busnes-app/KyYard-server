import { Check, Group, Lines, Rows, Text, type GroupProps } from './fields';

export function SecurityGroup({ spec, set }: GroupProps) {
  const risky = spec.privileged || spec.cap_add.some((c) => c.trim() !== '') || spec.devices.length > 0;
  return <Group title="Security">
    {risky && <p className="dr-alert dr-alert-warn">Privileged mode, added capabilities and host devices give this container control over parts of the host.</p>}
    <Check label="Privileged" checked={spec.privileged} onChange={(privileged) => set({ privileged })} />
    <Check label="Read-only root filesystem" checked={spec.read_only_rootfs} onChange={(read_only_rootfs) => set({ read_only_rootfs })} />
    <Check label="Run an init process" checked={spec.init} onChange={(init) => set({ init })} />
    <Lines label="Capabilities to add, one per line" value={spec.cap_add} onChange={(cap_add) => set({ cap_add })} />
    <Lines label="Capabilities to drop, one per line" value={spec.cap_drop} onChange={(cap_drop) => set({ cap_drop })} />
    <Lines label="Security options, one per line" value={spec.security_opt} onChange={(security_opt) => set({ security_opt })} />
    <Rows noun="Device" rows={spec.devices} blank={{ host: '', container: '', permissions: 'rwm' }} onChange={(devices) => set({ devices })} render={(d, update, label) => <>
      <input aria-label={`${label} host path`} placeholder="host path" value={d.host} onChange={(e) => update({ ...d, host: e.target.value })} />
      <input aria-label={`${label} container path`} placeholder="container path" value={d.container} onChange={(e) => update({ ...d, container: e.target.value })} />
      <input aria-label={`${label} permissions`} placeholder="permissions" value={d.permissions} onChange={(e) => update({ ...d, permissions: e.target.value })} />
    </>} />
  </Group>;
}

// Options are edited as rows so a key can be renamed; the form folds them into a sorted map.
export function LoggingGroup({ spec, set, options, onOptions }: GroupProps & { options: [string, string][]; onOptions: (rows: [string, string][]) => void }) {
  return <Group title="Logging">
    <Text label="Log driver" value={spec.log.driver} placeholder="daemon default" onChange={(driver) => set({ log: { ...spec.log, driver } })} />
    <Rows noun="Option" rows={options} blank={['', '']} onChange={onOptions} render={([k, v], update, label) => <>
      <input aria-label={`${label} name`} placeholder="option" value={k} onChange={(e) => update([e.target.value, v])} />
      <input aria-label={`${label} value`} placeholder="value" value={v} onChange={(e) => update([k, e.target.value])} />
    </>} />
  </Group>;
}
