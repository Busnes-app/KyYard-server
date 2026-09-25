import { ApplicationInspection, unsupportedNames, type InspectionTarget } from './ApplicationInspection';
import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';

type Blocker = 'mapping_requires_review' | 'unassigned_adopted_containers' | 'image_inventory_incomplete' | 'service_unmapped' | 'explicit_image_reference_required' | 'image_not_reported' | 'image_reference_ambiguous' | 'image_identity_invalid' | 'reported_port_overlap' | 'desired_port_overlap' | 'replacement_identity_invalid' | 'revision_services_differ' | 'bind_mount_new' | 'mounts_unreported' | 'mount_unsupported' | 'volume_missing'
  | 'clock_skew' | 'inspection_unavailable' | 'replacement_identity_changed' | 'configuration_unsupported' | 'frame_too_large' | 'too_many_registry_hosts' | 'frame_invalid' | 'agent_deploy_unsupported' | 'agent_pull_unsupported' | 'agent_inspect_unsupported' | 'kubernetes_unsupported' | 'k8s_namespace';
export const messages: Record<Blocker, string> = {
  mapping_requires_review: 'Review and save service mapping for the latest definition.',
  unassigned_adopted_containers: 'Some adopted containers are unassigned. Review service mapping before planning replacement.',
  image_inventory_incomplete: 'Image inventory is incomplete; image IDs cannot be resolved safely.',
  service_unmapped: 'Select an adopted container for this service.',
  explicit_image_reference_required: 'Save an explicit image tag or digest in the definition.',
  image_not_reported: 'This exact reference was not reported. Pull the required image or correct the reference, then refresh host inventory.',
  image_reference_ambiguous: 'This reference points to multiple reported image IDs. Use an unambiguous digest.',
  image_identity_invalid: 'The reported image ID is invalid. Refresh host inventory.',
  reported_port_overlap: 'A published port overlaps a reported binding outside the mapped containers.',
  desired_port_overlap: 'A published port overlaps another binding in the saved definition.',
  replacement_identity_invalid: 'A mapped container\'s recorded identity is incomplete. Release and adopt the project again before planning.',
  revision_services_differ: "The chosen revision's services differ from the mapped ones. Map against the latest definition or choose another revision.",
  bind_mount_new: 'This revision adds a host path the running container does not have; KyYard never introduces bind mounts. Mount it by hand first, or drop it from the definition.',
  mounts_unreported: "The agent has not reported this container's mounts, or reported only part of them. Upgrade the host agent to this release, then check again.",
  mount_unsupported: 'This container has mounts KyYard cannot recreate (anonymous volumes or unsupported mount types). Recreate it by hand with named volumes first.',
  volume_missing: 'An external volume this revision names does not exist on the host. Create it there first.',
  clock_skew: "The host's clock differs from the server's by more than five minutes. Correct the host clock (NTP), then check again.",
  inspection_unavailable: 'The live container could not be inspected before planning. Check that the host agent is connected, then plan again.',
  replacement_identity_changed: 'The live container is not the one in the stored inventory. Refresh the host inventory and plan again.',
  configuration_unsupported: 'The live container has configuration the definition cannot express, which a recreate would drop. Change it on the host, or recreate the container from the definition by hand, then plan again.',
  frame_too_large: 'This deployment is larger than the host agent accepts. Upgrade the agent, or reduce the environment values, then plan again.',
  too_many_registry_hosts: 'This deployment pulls with credentials from more than 16 registries. Pull from fewer, then plan again.',
  frame_invalid: 'This deployment could not be built from the saved definition and values. Review the definition, then plan again.',
  agent_deploy_unsupported: 'Upgrade the host agent to enable deployments.',
  agent_pull_unsupported: 'Upgrade the host agent to enable deployments that pull images.',
  agent_inspect_unsupported: 'Upgrade the host agent to enable live inspection, which planning requires.',
  kubernetes_unsupported: 'Some services cannot run as a Kubernetes Deployment yet; each is named below with the reason.',
  k8s_namespace: "The cluster's manifest no longer grants this namespace. Regenerate the manifest and apply it, or map the application to a granted namespace.",
};
// Definition volumes (named|bind) and runtime mounts (volume|bind); kinds render from this table only.
export type Mount = { kind: string; source: string; target: string; read_only?: boolean };
const mountKinds: Record<string, string> = { named: 'volume', volume: 'volume', bind: 'bind' };
export function MountList({ mounts }: { mounts: Mount[] }) {
  return <ul className="ky-list">{mounts.map((m, i) => <li key={i}><small>{Object.hasOwn(mountKinds, m.kind) ? mountKinds[m.kind] : 'other'}</small> <span>{`${m.source} → ${m.target}`}</span>{m.read_only && <> <span className="badge">ro</span></>}</li>)}</ul>;
}
// knownBlockers reads a 409 preflight_blocked body, keeping only the codes table describes.
export function knownBlockers<K extends string>(payload: unknown, table: Record<K, string>): K[] {
  const blockers = payload && typeof payload === 'object' ? (payload as { blockers?: unknown }).blockers : undefined;
  return Array.isArray(blockers) ? blockers.filter((b): b is K => typeof b === 'string' && Object.hasOwn(table, b)) : [];
}
// serviceFindings reads the services a 409 preflight_blocked body names and states, in fixed
// text, what a live inspection found for each: "web: runs privileged, maps host devices".
// Names outside the service grammar and unknown codes are dropped.
export function serviceFindings(payload: unknown): string[] {
  const services = payload && typeof payload === 'object' ? (payload as { services?: unknown }).services : undefined;
  if (!Array.isArray(services)) return [];
  const lines: string[] = [];
  for (const s of services) {
    if (!s || typeof s !== 'object') continue;
    const { name, blockers, unsupported } = s as { name?: unknown; blockers?: unknown; unsupported?: unknown };
    if (typeof name !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name)) continue;
    const codes = Array.isArray(unsupported) ? unsupported.filter((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) : [];
    if (codes.length) lines.push(`${name}: ${codes.map(c => unsupportedNames[c]).join(', ')}`);
    else if (Array.isArray(blockers) && blockers.includes('inspection_unavailable')) lines.push(`${name}: no live inspection answered`);
  }
  return lines;
}
type Preflight = { instance_id: string; endpoint_id: string; endpoint_name: string; revision: number; mapping_version: number; received_at: string; executable: boolean; blockers: Blocker[]; services: { name: string; reference: string; image_id: string; container_id: string; inspection_target?: InspectionTarget; blockers: Blocker[]; mounts?: Mount[]; dropped_mounts?: Mount[]; unsupported_mounts?: Mount[] }[] };
export function ApplicationPreflight({ base, instanceID, org }: { base: string; instanceID: string; org: string }) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close deployment preflight' : 'Deployment preflight'}</button>
    {open && <PreflightView key={`${org}/${base}/${instanceID}`} base={base} instanceID={instanceID} org={org} />}
  </section>;
}
function PreflightView({ base, instanceID, org }: { base: string; instanceID: string; org: string }) {
  const resource = useTenantResource<Preflight>(`${base}/preflight`);
  const [selected, setSelected] = useState<Preflight['services'][number] | null>(null);
  const data = resource.data;
  const page = usePagination(data?.services ?? [], base);
  return <>
    <p>Check confirmed service mappings, locally reported image IDs and published-port overlaps. All adopted identities must still be present in fresh, complete inventory. This does not pull images, approve deployment or change containers.</p>
    <StateNotice state={resource.state} onRetry={resource.reload} />
    {resource.state === 'ready' && data && (data.instance_id !== instanceID ? <p role="alert">Adoption changed. Refresh applications before running preflight.</p> : <>
      <h3>Deployment preflight</h3>
      <p>{data.endpoint_name} · definition revision {data.revision} · mapping version {data.mapping_version}</p>
      <p>Inventory received {new Date(data.received_at).toLocaleString()}</p>
      {data.executable ? <p>Ready to plan: every service maps to an adopted container and its image is known on the host.</p> : data.blockers.length === 0 ? <p>Not ready to plan.</p> : <ul className="ky-list">{data.blockers.map(b => <li key={b}>{messages[b] ?? 'Unrecognized preflight condition; planning is unavailable.'}</li>)}</ul>}
      <p>Image matches use exact references from this host's inventory. IDs are observations, not saved deployment pins. Port checks exclude mapped containers that a future replacement would stop, conservatively treat wildcard addresses as overlapping, and cannot detect host processes or unreported bindings.</p>
      <button type="button" className="btn-secondary" onClick={() => { setSelected(null); resource.reload(); }}>Refresh preflight</button>
      {page.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service / target</th><th>Local image</th><th>Mounts</th><th>Preflight findings</th></tr></thead><tbody>{page.rows.map(s => <tr key={s.name}>
        <td data-label="Service / target"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.container_id || 'Unmapped'}</small></div></td>
        <td data-label="Local image"><div className="ky-resource-name"><span>{s.reference}</span><small>{s.image_id || 'Unresolved'}</small></div></td>
        <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : 'No mounts.'}{s.dropped_mounts?.length ? <><p>Will be dropped by the recreate:</p><MountList mounts={s.dropped_mounts} /></> : null}{s.unsupported_mounts?.length ? <><p>Cannot be recreated:</p><MountList mounts={s.unsupported_mounts} /></> : null}</td>
        <td data-label="Preflight findings">{s.blockers.length ? <ul>{s.blockers.map(b => <li key={b}>{messages[b] ?? 'Unknown condition'}</li>)}</ul> : 'No findings.'}{s.inspection_target && <button type="button" className="btn-secondary" onClick={() => setSelected(s)} aria-label={`Inspect ${s.name}`}>Inspect live container</button>}</td>
      </tr>)}</tbody></table>
      {selected?.inspection_target && <ApplicationInspection key={`${data.endpoint_id}/${selected.name}`} org={org} endpoint={data.endpoint_id} service={selected.name} target={selected.inspection_target} onClose={() => setSelected(null)} />}
    </>)}
  </>;
}
