import { useState } from 'react';
import { useTenantResource } from '../tenant';
import { usePagination } from './Pagination';
import { StateNotice } from './StateNotice';

type Ownership = 'adopted' | 'missing' | 'identity_changed' | 'project_changed' | 'unowned';
type ImageComparison = 'same_reference' | 'different_reference' | 'unknown';
type Comparison = {
  instance_id: string; endpoint_name: string; project: string; revision: number; adopted_revision: number; digest: string;
  received_at: string | null; availability: 'available' | 'unavailable' | 'stale' | 'incomplete';
  services: { name: string; image: string; observed: number }[];
  containers: { id: string; name: string; ownership: Ownership; service: string; image: string; image_comparison: ImageComparison; state: string }[];
};
const ownershipText: Record<Ownership, string> = { adopted: 'Adopted identity present', missing: 'Adopted container missing', identity_changed: 'Identity changed', project_changed: 'Project changed', unowned: 'Not adopted' };
const imageText: Record<ImageComparison, string> = { same_reference: 'Same image reference', different_reference: 'Different image reference', unknown: 'Image comparison unknown' };
const availabilityText = { unavailable: 'Host or inventory unavailable.', stale: 'Inventory is stale or its clock is out of range.', incomplete: 'Inventory is incomplete. Missing containers cannot be determined.' };

export function ApplicationComparison({ base, instanceID }: { base: string; instanceID: string }) {
  const [open, setOpen] = useState(false);
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close comparison' : 'Compare with host'}</button>
    {open && <ComparisonView base={base} instanceID={instanceID} />}
  </section>;
}
function ComparisonView({ base, instanceID }: { base: string; instanceID: string }) {
  const resource = useTenantResource<Comparison>(`${base}/comparison`);
  const data = resource.data;
  const containers = usePagination(data?.containers ?? [], base);
  const services = usePagination(data?.services ?? [], base);
  return <>
    <StateNotice state={resource.state} onRetry={resource.reload} />
    {resource.state === 'ready' && data && (data.instance_id !== instanceID ? <p role="alert">Adoption changed. Refresh applications before comparing again.</p> : <>
      <h3>Observed comparison · {data.endpoint_name}</h3>
      <p>Project <bdi>{data.project}</bdi> · desired revision {data.revision} · adopted at revision {data.adopted_revision}</p>
      <p>This is a read-only observation, not a deployment preview. Service labels suggest matches but do not grant ownership. Matching image references do not verify image content. Ports, environment, restart policy, mounts and networks are not compared.</p>
      {data.received_at && <p>Inventory received {new Date(data.received_at).toLocaleString()}</p>}
      <button type="button" className="btn-secondary" onClick={resource.reload}>Refresh comparison</button>
      {data.availability !== 'available' ? <p role="alert">{availabilityText[data.availability]} Refresh the host inventory before comparing.</p> : <>
        <h3>Desired services</h3>
        <p>Counts include only unchanged adopted identities with a matching service label. Zero does not prove the service is absent: labels may be missing or trimmed.</p>
        <ul className="ky-list">{services.rows.map((s) => <li key={s.name}><strong>{s.name}</strong> · {s.image} · {s.observed} observed</li>)}</ul>
        {services.controls}
        <h3>Container observations</h3>
        {containers.controls}
        <table className="ky-table ky-responsive-table"><thead><tr><th>Container</th><th>Ownership</th><th>Service / image</th></tr></thead><tbody>{containers.rows.map((c) => <tr key={c.id}>
          <td data-label="Container"><div className="ky-resource-name"><strong>{c.name}</strong><small>{c.id}</small><small>{c.state || 'State unknown'}</small></div></td>
          <td data-label="Ownership">{ownershipText[c.ownership]}</td>
          <td data-label="Service / image"><div className="ky-resource-name"><span>{c.service || 'Service label unknown'}</span><span>{c.image || 'Image unknown'}</span><small>{imageText[c.image_comparison]}</small></div></td>
        </tr>)}</tbody></table>
      </>}
    </>)}
  </>;
}
