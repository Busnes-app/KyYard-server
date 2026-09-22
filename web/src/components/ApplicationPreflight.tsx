import { ApplicationInspection, type InspectionTarget } from './ApplicationInspection';
import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { StateNotice } from './StateNotice';
import { usePagination } from './Pagination';

type Blocker = 'runtime_verification_required' | 'mapping_requires_review' | 'unassigned_adopted_containers' | 'image_inventory_incomplete' | 'service_unmapped' | 'explicit_image_reference_required' | 'image_not_reported' | 'image_reference_ambiguous' | 'image_identity_invalid' | 'reported_port_overlap' | 'desired_port_overlap' | 'replacement_identity_invalid';
export const messages: Record<Blocker, string> = {
  runtime_verification_required: 'Deployment execution is not available yet. Live inspection provides selected observations; configuration parity, secrets and safe replacement remain unverified.',
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
};
type Preflight = { instance_id: string; endpoint_id: string; endpoint_name: string; revision: number; mapping_version: number; received_at: string; executable: boolean; blockers: Blocker[]; services: { name: string; reference: string; image_id: string; container_id: string; inspection_target?: InspectionTarget; blockers: Blocker[] }[] };
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
      <h3>Deployment is not enabled</h3>
      <p>{data.endpoint_name} · definition revision {data.revision} · mapping version {data.mapping_version}</p>
      <p>Inventory received {new Date(data.received_at).toLocaleString()}</p>
      <ul className="ky-list">{data.blockers.map(b => <li key={b}>{messages[b] ?? 'Unrecognized preflight condition; deployment remains unavailable.'}</li>)}</ul>
      <p>Image matches use exact references from this host's inventory. IDs are observations, not saved deployment pins. Port checks exclude mapped containers that a future replacement would stop, conservatively treat wildcard addresses as overlapping, and cannot detect host processes or unreported bindings.</p>
      <button type="button" className="btn-secondary" onClick={() => { setSelected(null); resource.reload(); }}>Refresh preflight</button>
      {page.controls}
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service / target</th><th>Local image</th><th>Preflight findings</th></tr></thead><tbody>{page.rows.map(s => <tr key={s.name}>
        <td data-label="Service / target"><div className="ky-resource-name"><strong>{s.name}</strong><small>{s.container_id || 'Unmapped'}</small></div></td>
        <td data-label="Local image"><div className="ky-resource-name"><span>{s.reference}</span><small>{s.image_id || 'Unresolved'}</small></div></td>
        <td data-label="Preflight findings">{s.blockers.length ? <ul>{s.blockers.map(b => <li key={b}>{messages[b] ?? 'Unknown condition'}</li>)}</ul> : 'No additional inventory findings; runtime verification still required.'}{s.inspection_target && <button type="button" className="btn-secondary" onClick={() => setSelected(s)} aria-label={`Inspect ${s.name}`}>Inspect live container</button>}</td>
      </tr>)}</tbody></table>
      {selected?.inspection_target && <ApplicationInspection key={`${data.endpoint_id}/${selected.name}`} org={org} endpoint={data.endpoint_id} service={selected.name} target={selected.inspection_target} onClose={() => setSelected(null)} />}
    </>)}
  </>;
}
