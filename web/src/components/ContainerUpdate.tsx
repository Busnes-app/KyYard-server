import { useEffect, useRef, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { secureFetch } from '../api';
import type { Container } from '../tenant';
import { containerPath } from '../router';
import { Link } from './Link';
const VERDICTS: Record<string, string> = { up_to_date: 'Up to date', update_available: 'Update available', pinned: 'Digest pinned', unknown: 'Image version unknown', registry_error: 'Registry check failed' };
export function ContainerUpdate({ base, container, active, org, endpoint }: { base: string; container: Container; active: boolean; org: string; endpoint: string }) {
  const [verdict, setVerdict] = useState('');
  const [busy, setBusy] = useState(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  async function check() {
    setBusy(true);
    try {
      const response = await secureFetch(`${base}/containers/${encodeURIComponent(container.id)}/updates/check`, { method: 'POST' });
      const body: unknown = response.ok ? await response.json() : null;
      if (!alive.current) return;
      if (body && typeof body === 'object' && 'image_id' in body && body.image_id === container.image_id && 'verdict' in body && typeof body.verdict === 'string' && Object.hasOwn(VERDICTS, body.verdict)) setVerdict(body.verdict);
      else setVerdict('registry_error');
    } catch { if (alive.current) setVerdict('registry_error'); }
    finally { if (alive.current) setBusy(false); }
  }
  return <span className="ky-image-update"><span className={`badge ${verdict === 'update_available' ? 'badge-warning' : 'badge-secondary'}`}>{busy ? 'Checking…' : VERDICTS[verdict] ?? 'Updates not checked'}</span><button type="button" className="btn-secondary ky-icon-button" aria-label={`Check image update for ${container.name}`} title="Check image update" disabled={!active || busy} onClick={() => void check()}><RefreshCw size={15} /></button>{active && <Link to={containerPath(org, endpoint, container.id, 'configuration')}>Update image</Link>}</span>;
}
