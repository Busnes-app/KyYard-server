import { useEffect, useState } from 'react';
import { tenantWrite, useTenantResource, type UpdateCheck } from '../tenant';
import { StateNotice } from './StateNotice';

const verdictText: Record<string, string> = { current: 'Up to date', update_available: 'Update available', pinned: 'Pinned', unknown_local: 'Unknown on host', registry_error: 'Registry error' };
const detailText: Record<string, string> = {
  not_configured: 'No registry entry for this host and anonymous pulls are off.',
  unauthorized: 'The registry refused the credentials.',
  not_found: 'The image was not found in the registry.',
  rate_limited: 'The registry rate limit was reached; try later.',
  private_destination: 'The registry is on a private address this organization may not reach.',
  unavailable: 'The registry could not be reached.',
};
const texts = {
  forbidden: 'Only administrators and developers can check for updates.',
  conflict: {
    check_in_progress: 'A check is already running.',
    mapping_required: 'Map the services first.',
    adoption_changed: 'Adoption changed. Refresh applications before checking.',
  },
};
// Server strings never render raw: unknown verdicts and details show nothing.
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';
const short = (d: string) => /^sha256:[0-9a-f]{64}$/.test(d) ? d.slice(7, 19) : '—';

type Props = { base: string; instanceID: string; mappingVersion: number };

export function ApplicationUpdates(props: Props) {
  const [open, setOpen] = useState(false);
  if (props.mappingVersion < 1) return null;
  return <section className="dr-stack" style={{ overflowWrap: 'anywhere' }}>
    <button type="button" className="btn-secondary" onClick={() => setOpen(!open)}>{open ? 'Close updates' : 'Updates'}</button>
    {open && <UpdatesView key={`${props.base}/${props.instanceID}`} {...props} />}
  </section>;
}
function UpdatesView({ base, instanceID }: Props) {
  const checks = useTenantResource<UpdateCheck>(`${base}/updates`);
  // The last ready read stays on screen while a post-check reload is in flight.
  const [shown, setShown] = useState<UpdateCheck | null>(null);
  useEffect(() => { if (checks.state === 'ready') setShown(checks.data); }, [checks.state, checks.data]);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const check = async () => {
    setBusy(true);
    const err = await tenantWrite(`${base}/updates/check`, 'POST', undefined, texts);
    setBusy(false);
    setMessage(err);
    if (!err) checks.reload();
  };
  const visible = checks.state === 'ready' || (checks.state === 'loading' && shown !== null);
  const data = checks.state === 'ready' ? checks.data : shown;
  const rows = data?.instance_id === instanceID ? data.services : [];
  return <>
    <p>Compares each mapped service's image with its registry. Nothing is pulled or deployed.</p>
    <button type="button" className="btn-secondary" disabled={busy} onClick={() => void check()}>Check for updates</button>
    {message && <p role="alert">{message}</p>}
    {!visible && <StateNotice state={checks.state} onRetry={checks.reload} />}
    {visible && (rows.length === 0 ? <p>No update check yet.</p> : <table>
      <thead><tr><th>Service</th><th>Reference</th><th>Status</th><th>On host</th><th>In registry</th><th>Checked</th></tr></thead>
      <tbody>{rows.map((r) => <tr key={r.service}>
        <td>{r.service}</td>
        <td>{r.reference}</td>
        <td><span className="badge">{fixed(verdictText, r.verdict)}</span>{r.verdict === 'registry_error' && fixed(detailText, r.detail) && <div>{fixed(detailText, r.detail)}</div>}</td>
        <td><code>{short(r.local_digest)}</code></td>
        <td><code>{short(r.remote_digest)}</code></td>
        <td>{new Date(r.checked_at).toLocaleString()}</td>
      </tr>)}</tbody>
    </table>)}
  </>;
}
