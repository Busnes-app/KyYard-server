import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Inventory } from '../tenant';
import type { ApplicationInstance } from './ApplicationAdoption';
import type { Revision } from './Applications';
import { usePagination } from './Pagination';
import { StateNotice } from './StateNotice';

type Props = { base: string; org: string; instance: ApplicationInstance; expected: number; editable: boolean; onSaved: () => void };

export function KubernetesNetworking(props: Props) {
  const [open, setOpen] = useState(false);
  // Unknown writes stay blocked across closing/reopening; refresh applications to review.
  const [blocked, setBlocked] = useState(false);
  return <section className="dr-stack" aria-label="Internal networking">
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>Internal networking</button>
    {open && <Networking {...props} blocked={blocked} onBlocked={() => setBlocked(true)} />}
  </section>;
}

function Networking({ base, org, instance, expected, editable, onSaved, blocked, onBlocked }: Props & { blocked: boolean; onBlocked: () => void }) {
  const revision = useTenantResource<Revision>(`${base}/revisions/${expected}`);
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(instance.endpoint_id)}/inventory`);
  return <>
    <p>Give each service a fixed internal IP for Nginx or cloudflared running in this cluster. Choose an address from the cluster's Service IP range. Kubernetes checks availability when you apply.</p>
    <StateNotice state={revision.state} onRetry={revision.reload} />
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.data && <p>Last inventory: {new Date(inventory.data.received_at).toLocaleString()}{inventory.refreshFailed ? ' · Last refresh failed.' : ''}{inventory.data.snapshot.truncated?.includes('services') ? ' · Service inventory is incomplete.' : ''}</p>}
    <button type="button" className="btn-secondary" onClick={inventory.reload}>Refresh backend addresses</button>
    {revision.state === 'ready' && revision.data && <NetworkForm key={revision.data.digest} base={base} expected={expected} revision={revision.data} inventory={inventory.state === 'ready' ? inventory.data ?? undefined : undefined} instance={instance} editable={editable} blocked={blocked} onBlocked={onBlocked} onSaved={onSaved} />}
  </>;
}

function NetworkForm({ base, expected, revision, inventory, instance, editable, blocked, onBlocked, onSaved }: Pick<Props, 'base' | 'expected' | 'instance' | 'editable' | 'onSaved'> & { revision: Revision; inventory?: Inventory; blocked: boolean; onBlocked: () => void }) {
  const [ips, setIPs] = useState<Record<string, string>>(revision.spec.kubernetes?.service_ips ?? {});
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const services = usePagination(revision.spec.services, `${base}/${expected}`);
  const save = async () => {
    setBusy(true); setMessage('');
    const serviceIPs = Object.fromEntries(Object.entries(ips).filter(([, ip]) => ip.trim()).map(([service, ip]) => [service, ip.trim()]));
    try {
      const response = await secureFetch(`${base}/networking`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expected_revision: expected, service_ips: serviceIPs }) });
      if (response.ok) { onSaved(); return; }
      if (response.status === 400) setMessage('Check the IP addresses: each must be a distinct IPv4 or canonical IPv6 address for a service with a published port.');
      else if (response.status === 403) setMessage('You do not have permission to edit this application.');
      else { onBlocked(); setMessage('The revision changed or the save outcome is unknown. Refresh applications and review the saved revision before trying again.'); }
    } catch { onBlocked(); setMessage('The save outcome is unknown. Refresh applications and review the saved revision before trying again.'); }
    finally { setBusy(false); }
  };
  return <>
    <p>Blank means automatic allocation for a new Service; existing Services keep their IP. To pin an existing Service, enter its assigned IP. Saving creates a revision and preserves environment values; plan and apply it separately.</p>
    {services.controls}
    <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Requested internal IP</th><th>Reported proxy backend</th></tr></thead><tbody>{services.rows.map((s) => {
        const observed = inventory?.snapshot.kubernetes?.services.find((v) => v.namespace === instance.namespace && v.instance === instance.id && v.service === s.name);
        const host = observed?.cluster_ip.includes(':') ? `[${observed.cluster_ip}]` : observed?.cluster_ip;
        return <tr key={s.name}>
          <td data-label="Service">{s.name}</td>
          <td data-label="Requested internal IP">{s.ports?.length ? <input aria-label={`Internal IP for ${s.name}`} value={ips[s.name] ?? ''} maxLength={45} autoComplete="off" spellCheck={false} placeholder="Automatic" disabled={busy || blocked || !editable} onChange={(e) => setIPs({ ...ips, [s.name]: e.target.value })} /> : 'Publish a port in the definition first.'}</td>
          <td data-label="Reported proxy backend" style={{ overflowWrap: 'anywhere' }}>{host && host !== 'None' ? observed?.ports.map((port) => {
            const match = /^(\d+)(?::\d+)?\/(TCP|UDP)$/.exec(port);
            if (!match) return null;
            return <div key={port}><code>{match[2] === 'TCP' ? `http://${host}:${match[1]}` : `${host}:${match[1]} (UDP)`}</code></div>;
          }) : 'No assigned backend reported yet.'}</td>
        </tr>;
      })}</tbody></table>
      <p>HTTP URLs assume the backend serves HTTP; use https:// if it serves TLS. These addresses are reachable inside the cluster. Proxy configuration and access restrictions are managed separately.</p>
      {editable && <button type="submit" disabled={busy || blocked}>Save networking as revision {expected + 1}</button>}
      {(message || blocked) && <p role="status">{message || 'Refresh applications and review the saved revision before trying again.'}</p>}
    </form>
  </>;
}
