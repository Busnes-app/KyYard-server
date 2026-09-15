import React, { useEffect, useState } from 'react';
import {
  AlertCircle,
  CheckCircle2,
  Clock,
  Download,
  HardDrive,
  KeyRound,
  Link2,
  Loader2,
  Play,
  RefreshCw,
  Send,
  Server,
  Unlink,
  XCircle,
} from 'lucide-react';
import { secureFetch } from '../api';

export interface DepositReceipt {
  capsule_id: string;
  digest: string;
  size_bytes: number;
  deposited_at: string;
}

export interface LocalCopy {
  name: string;
  size_bytes: number;
  created_at: string;
}

export interface BackupStatus {
  paired: boolean;
  key_pinned: boolean;
  app_name: string;
  app_version: string;
  allow_private_recovery: boolean;
  database_driver: string;
  members: string[];
  recovery_url?: string;
  recovery_key_id?: string;
  recovery_key_error?: string;
  threshold?: number;
  total_shares?: number;
  last_deposit?: DepositReceipt;
  local_dir?: string;
  local_keep?: number;
  local_copies?: LocalCopy[];
  local_error?: string;
  interval_sec?: number;
  min_interval_sec?: number;
  next_run_at?: string;
}

interface DrillCheck {
  name: string;
  passed: boolean;
  message?: string;
}

interface DrillResult {
  passed: boolean;
  checks: DrillCheck[];
  error_message?: string;
  duration_ms: number;
}

interface RunResult {
  manifest: { capsule_id: string };
  size_bytes: number;
  local_path?: string;
  local_error?: string;
  receipt?: DepositReceipt;
  receipt_unrecorded?: boolean;
}

const HOUR = 3600;
const SCHEDULE_CHOICES: { label: string; sec: number }[] = [
  { label: 'Off', sec: 0 },
  { label: 'Every hour', sec: HOUR },
  { label: 'Every 6 hours', sec: 6 * HOUR },
  { label: 'Every 12 hours', sec: 12 * HOUR },
  { label: 'Daily', sec: 24 * HOUR },
  { label: 'Weekly', sec: 7 * 24 * HOUR },
];

function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
}

function every(sec?: number): string {
  if (!sec) return 'Off';
  const hit = SCHEDULE_CHOICES.find((c) => c.sec === sec);
  if (hit) return hit.label;
  return sec % HOUR === 0 ? `Every ${sec / HOUR} hours` : `Every ${Math.round(sec / 60)} minutes`;
}

function bytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KiB`;
  return `${n} B`;
}

/** Reads the server's JSON error, or a status-only message when the body is not JSON. */
async function apiError(res: Response, fallback: string): Promise<Error> {
  const body: unknown = await res.json().catch(() => ({}));
  const message =
    typeof body === 'object' && body !== null && 'error' in body
      ? String((body as { error: unknown }).error)
      : `${fallback} (HTTP ${res.status})`;
  return new Error(message);
}

async function call<T>(path: string, init: RequestInit, fallback: string): Promise<T> {
  const res = await secureFetch(path, { credentials: 'same-origin', ...init });
  if (!res.ok) throw await apiError(res, fallback);
  return (await res.json()) as T;
}

const errorText = (err: unknown, fallback: string): string => (err instanceof Error && err.message ? err.message : fallback);

const Alert: React.FC<{ kind: 'error' | 'warn' | 'success'; children: React.ReactNode }> = ({ kind, children }) => (
  <div className={`dr-alert dr-alert-${kind}`} role={kind === 'success' ? 'status' : 'alert'}>
    {kind === 'success' ? <CheckCircle2 size={16} /> : kind === 'error' ? <XCircle size={16} /> : <AlertCircle size={16} />}
    <span>{children}</span>
  </div>
);

const Badge: React.FC<{ tone: 'success' | 'accent' | 'danger' | 'muted'; children: React.ReactNode }> = ({ tone, children }) => (
  <span className={tone === 'muted' ? 'badge dr-badge-muted' : `badge badge-${tone}`}>{children}</span>
);

export const Backup: React.FC = () => {
  const [status, setStatus] = useState<BackupStatus | null>(null);
  const [loadingStatus, setLoadingStatus] = useState<boolean>(true);
  const [statusError, setStatusError] = useState<string>('');

  const [running, setRunning] = useState<boolean>(false);
  const [runMessage, setRunMessage] = useState<string>('');
  const [runError, setRunError] = useState<string>('');

  const [runningDrill, setRunningDrill] = useState<boolean>(false);
  const [drillResult, setDrillResult] = useState<DrillResult | null>(null);

  const [scheduleSec, setScheduleSec] = useState<number>(24 * HOUR);
  const [scheduleSaving, setScheduleSaving] = useState<boolean>(false);
  const [scheduleMessage, setScheduleMessage] = useState<string>('');
  const [scheduleError, setScheduleError] = useState<string>('');

  const [remoteUrl, setRemoteUrl] = useState<string>('');
  const [pairCode, setPairCode] = useState<string>('');
  const [pairing, setPairing] = useState<boolean>(false);
  const [pairMessage, setPairMessage] = useState<string>('');
  const [pairError, setPairError] = useState<string>('');
  const [unpairing, setUnpairing] = useState<boolean>(false);

  const [pinKey, setPinKey] = useState<string>('');
  const [pinK, setPinK] = useState<string>('2');
  const [pinN, setPinN] = useState<string>('3');
  const [pinning, setPinning] = useState<boolean>(false);
  const [pinError, setPinError] = useState<string>('');

  const fetchStatus = async () => {
    setLoadingStatus(true);
    setStatusError('');
    try {
      const data = await call<BackupStatus>('/api/backup/status', { method: 'GET' }, 'Could not load backup status');
      setStatus(data);
      if (data.recovery_url) setRemoteUrl(data.recovery_url);
      if (typeof data.interval_sec === 'number') setScheduleSec(data.interval_sec);
    } catch (err) {
      setStatusError(errorText(err, 'Could not load backup status'));
    } finally {
      setLoadingStatus(false);
    }
  };

  useEffect(() => {
    void fetchStatus();
  }, []);

  const runBackup = async () => {
    setRunning(true);
    setRunMessage('');
    setRunError('');
    try {
      const res = await call<RunResult>('/api/backup/deposit', { method: 'POST' }, 'Backup failed');
      const went: string[] = [];
      if (res.local_path) went.push(`written to ${res.local_path}`);
      if (res.receipt) went.push(`deposited with KyRecovery at ${when(res.receipt.deposited_at)}`);
      setRunMessage(`Capsule ${res.manifest.capsule_id} (${bytes(res.size_bytes)}) ${went.join(' and ')}.`);
      if (res.local_error) setRunError(`The local copy failed: ${res.local_error}`);
      if (res.receipt_unrecorded) setRunError('KyRecovery holds the capsule but the receipt could not be recorded here; check the audit log.');
    } catch (err) {
      setRunError(errorText(err, 'Backup failed'));
    } finally {
      setRunning(false);
      await fetchStatus();
    }
  };

  /** Export is a CSRF-protected POST, so the file is fetched and saved from a blob. */
  const downloadCapsule = async () => {
    setRunError('');
    setRunMessage('');
    try {
      const res = await secureFetch('/api/backup/export-capsule', { method: 'POST', credentials: 'same-origin' });
      if (!res.ok) throw await apiError(res, 'Download failed');
      const match = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') ?? '');
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement('a');
      a.href = url;
      a.download = match ? match[1] : 'backup.kycap';
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      setRunError(errorText(err, 'Could not download the capsule'));
    }
  };

  const runDrill = async () => {
    setRunningDrill(true);
    setDrillResult(null);
    try {
      setDrillResult(await call<DrillResult>('/api/backup/drill', { method: 'POST' }, 'Restore drill failed to run'));
    } catch (err) {
      const message = errorText(err, 'Restore drill failed to run');
      setDrillResult({ passed: false, duration_ms: 0, error_message: message, checks: [{ name: 'Execution', passed: false, message }] });
    } finally {
      setRunningDrill(false);
    }
  };

  const saveSchedule = async (e: React.FormEvent) => {
    e.preventDefault();
    setScheduleSaving(true);
    setScheduleMessage('');
    setScheduleError('');
    try {
      const saved = await call<{ interval_sec: number }>(
        '/api/backup/schedule',
        { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ interval_sec: scheduleSec }) },
        'Could not save the schedule',
      );
      setScheduleMessage(saved.interval_sec === 0 ? 'Automatic backups are off.' : `Backing up ${every(saved.interval_sec).toLowerCase()}.`);
      await fetchStatus();
    } catch (err) {
      setScheduleError(errorText(err, 'Could not save the schedule'));
    } finally {
      setScheduleSaving(false);
    }
  };

  const pair = async (e: React.FormEvent) => {
    e.preventDefault();
    setPairing(true);
    setPairMessage('');
    setPairError('');
    try {
      await call(
        '/api/backup/pair-remote',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ recovery_url: remoteUrl.trim(), pairing_code: pairCode.trim() }),
        },
        'Pairing failed',
      );
      setPairMessage(`Paired with ${remoteUrl.trim()}.`);
      setPairCode('');
      await fetchStatus();
    } catch (err) {
      setPairError(errorText(err, 'Pairing failed'));
    } finally {
      setPairing(false);
    }
  };

  const unpair = async () => {
    if (
      !window.confirm(
        'Unpair from KyRecovery?\n\nRemoves the URL and sealed token rows. The key pin, receipts and local copies stay. The credential is dead only when the KyRecovery admin revokes it.',
      )
    )
      return;
    setUnpairing(true);
    setPairMessage('');
    setPairError('');
    try {
      await call('/api/backup/pairing', { method: 'DELETE' }, 'Could not unpair');
      setPairMessage('Unpaired. Off-site backups have stopped; ask the KyRecovery admin to revoke this service there.');
      await fetchStatus();
    } catch (err) {
      setPairError(errorText(err, 'Could not unpair'));
    } finally {
      setUnpairing(false);
    }
  };

  const pin = async (e: React.FormEvent) => {
    e.preventDefault();
    setPinning(true);
    setPinError('');
    try {
      await call(
        '/api/backup/pin-key',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ public_key: pinKey.trim(), threshold: Number(pinK), total_shares: Number(pinN) }),
        },
        'Could not pin the key',
      );
      setPinKey('');
      await fetchStatus();
    } catch (err) {
      setPinError(errorText(err, 'Could not pin the key'));
    } finally {
      setPinning(false);
    }
  };

  const keyPinned = status?.key_pinned ?? false;
  const paired = status?.paired ?? false;
  const hasLocal = Boolean(status?.local_dir);
  const canBackUp = keyPinned && (paired || hasLocal);
  const scheduleOn = (status?.interval_sec ?? 0) > 0;
  const copies = status?.local_copies ?? [];
  const newestLocal = copies[0];

  return (
    <div className="dr-page">
      <div className="dr-header">
        <h1>Backup &amp; recovery</h1>
        <button type="button" className="btn-secondary" onClick={fetchStatus} disabled={loadingStatus}>
          <RefreshCw size={14} className={loadingStatus ? 'animate-spin' : ''} />
          <span>Refresh</span>
        </button>
      </div>

      {statusError && <Alert kind="error">{statusError}</Alert>}
      {status?.recovery_key_error && <Alert kind="error">{status.recovery_key_error}</Alert>}
      {status && status.database_driver !== 'sqlite' && (
        <Alert kind="warn">
          Backups do not include the PostgreSQL database: a capsule can only snapshot SQLite. Back this database up
          with pg_dump on its own schedule.
        </Alert>
      )}
      {status && !keyPinned && <Alert kind="warn">No backups are being made. Pair with KyRecovery or pin the suite recovery key below.</Alert>}
      {status && keyPinned && !paired && !hasLocal && (
        <Alert kind="warn">A key is pinned but capsules have nowhere to go. Pair with KyRecovery, or set KY_BACKUP_DIR to keep copies on this host.</Alert>
      )}
      {status && keyPinned && !scheduleOn && <Alert kind="warn">Automatic backups are off. Only the button below makes one.</Alert>}

      <div className="dr-facts">
        <div className="dr-fact">
          <div className="dr-fact-label">
            <span>Recovery key</span>
            <Badge tone={keyPinned ? 'success' : 'danger'}>{keyPinned ? 'Pinned' : 'None'}</Badge>
          </div>
          <div className="dr-fact-value dr-mono">{status?.recovery_key_id ?? '—'}</div>
          <div className="dr-fact-note">
            {keyPinned ? `${status?.threshold} of ${status?.total_shares} custodian cards open a capsule` : 'Nothing can be sealed until a key is pinned'}
          </div>
        </div>
        <div className="dr-fact">
          <div className="dr-fact-label">
            <span>KyRecovery</span>
            <Badge tone={paired ? 'success' : 'muted'}>{paired ? 'Paired' : 'Not paired'}</Badge>
          </div>
          <div className="dr-fact-value">{paired ? status?.recovery_url : 'No off-site copy'}</div>
          <div className="dr-fact-note">
            {status?.last_deposit ? `Last deposit ${when(status.last_deposit.deposited_at)}` : paired ? 'Nothing deposited yet' : ''}
          </div>
        </div>
        <div className="dr-fact">
          <div className="dr-fact-label">
            <span>Local copies</span>
            <Badge tone={hasLocal ? 'success' : 'muted'}>{hasLocal ? `${copies.length} of ${status?.local_keep}` : 'Off'}</Badge>
          </div>
          <div className="dr-fact-value dr-mono">{status?.local_dir ?? 'KY_BACKUP_DIR not set'}</div>
          <div className="dr-fact-note">{status?.local_error ?? (newestLocal ? `Newest ${when(newestLocal.created_at)}` : hasLocal ? 'Nothing written yet' : '')}</div>
        </div>
        <div className="dr-fact">
          <div className="dr-fact-label">
            <span>Schedule</span>
            <Badge tone={scheduleOn ? 'success' : 'danger'}>{every(status?.interval_sec)}</Badge>
          </div>
          <div className="dr-fact-value">{scheduleOn && status?.next_run_at ? `Next ${when(status.next_run_at)}` : 'Manual only'}</div>
          <div className="dr-fact-note">Counts from the last attempt, successful or not</div>
        </div>
      </div>

      <div className="panel dr-section">
        <div className="panel-header">
          <h3>Back up now</h3>
          <div className="dr-actions">
            <button type="button" onClick={runBackup} disabled={running || !canBackUp}>
              {running ? <Loader2 size={14} className="animate-spin" /> : <Send size={14} />}
              <span>{running ? 'Sealing…' : 'Back up now'}</span>
            </button>
            <button type="button" className="btn-secondary" onClick={downloadCapsule} disabled={!keyPinned}>
              <Download size={14} />
              <span>Download capsule</span>
            </button>
            <button type="button" className="btn-secondary" onClick={runDrill} disabled={runningDrill}>
              {runningDrill ? <Loader2 size={14} className="animate-spin" /> : <Play size={14} />}
              <span>{runningDrill ? 'Restoring…' : 'Run restore drill'}</span>
            </button>
          </div>
        </div>
        <p className="dr-hint">
          One sealed capsule goes to every destination above: the local directory, and KyRecovery when paired. Download saves the same
          capsule to this browser instead. Nothing on this server can open a capsule; that takes {status?.threshold ?? 'k'} custodian cards
          together.
        </p>
        <div className="dr-hint">A capsule carries</div>
        <ul className="dr-members">
          {(status?.members ?? []).map((m) => (
            <li key={m}>{m}</li>
          ))}
        </ul>
        {runMessage && <Alert kind="success">{runMessage}</Alert>}
        {runError && <Alert kind="error">{runError}</Alert>}
        {copies.length > 0 && (
          <ul className="dr-copies">
            {copies.map((c) => (
              <li key={c.name}>
                <span>{c.name}</span>
                <span>
                  {bytes(c.size_bytes)} · {when(c.created_at)}
                </span>
              </li>
            ))}
          </ul>
        )}
        {drillResult && (
          <div className="dr-drill">
            <div>
              Restore drill <Badge tone={drillResult.passed ? 'success' : 'danger'}>{drillResult.passed ? 'passed' : 'failed'}</Badge>{' '}
              <span className="dr-hint">{drillResult.duration_ms} ms</span>
            </div>
            {drillResult.error_message && <div className="dr-danger">{drillResult.error_message}</div>}
            <div className="dr-checks">
              {drillResult.checks.map((check, idx) => (
                <div key={idx} className="dr-check">
                  {check.passed ? <CheckCircle2 size={14} className="dr-ok" /> : <XCircle size={14} className="dr-danger" />}
                  <strong>{check.name}</strong>
                  <span className="dr-hint">{check.message}</span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>

      <div className="panel dr-section">
        <div className="panel-header">
          <h3>Schedule</h3>
        </div>
        <form onSubmit={saveSchedule} className="dr-row">
          <label className="dr-field">
            <span>Back up automatically</span>
            <select value={scheduleSec} onChange={(e) => setScheduleSec(Number(e.target.value))}>
              {SCHEDULE_CHOICES.map((c) => (
                <option key={c.sec} value={c.sec}>
                  {c.label}
                </option>
              ))}
            </select>
          </label>
          <button type="submit" disabled={scheduleSaving || scheduleSec === (status?.interval_sec ?? -1)}>
            {scheduleSaving ? <Loader2 size={14} className="animate-spin" /> : <Clock size={14} />}
            <span>Save</span>
          </button>
        </form>
        <p className="dr-hint">
          Each run snapshots the whole database, so the floor is {Math.round((status?.min_interval_sec ?? 900) / 60)} minutes. The schedule does
          nothing until a key is pinned and there is somewhere to send the capsule. KY_BACKUP_DEPOSIT_INTERVAL is only the default.
        </p>
        {scheduleMessage && <Alert kind="success">{scheduleMessage}</Alert>}
        {scheduleError && <Alert kind="error">{scheduleError}</Alert>}
      </div>

      <div className="dr-two">
        <div className="panel dr-section">
          <div className="panel-header">
            <h3>
              <Server size={16} /> KyRecovery
            </h3>
            <Badge tone={paired ? 'success' : 'muted'}>{paired ? 'Paired' : 'Not paired'}</Badge>
          </div>
          <p className="dr-hint">
            KyRecovery keeps capsules it cannot open, off this host. In its dashboard, generate a pairing code for this service and enter it
            here. Pairing hands this server the suite recovery key and a deposit credential;
            {paired ? ' re-pairing is only accepted with the same key.' : ' the key is pinned once and never replaced.'}
          </p>
          <form onSubmit={pair} className="dr-stack">
            <label className="dr-field">
              <span>Server URL</span>
              <input type="url" placeholder="https://recovery.example.com" value={remoteUrl} onChange={(e) => setRemoteUrl(e.target.value)} required />
            </label>
            <div className="dr-row">
              <label className="dr-field">
                <span>Pairing code</span>
                <input
                  className="dr-mono"
                  type="text"
                  inputMode="numeric"
                  placeholder="123456"
                  value={pairCode}
                  onChange={(e) => setPairCode(e.target.value)}
                  required
                />
              </label>
              <button type="submit" disabled={pairing}>
                {pairing ? <Loader2 size={14} className="animate-spin" /> : <Link2 size={14} />}
                <span>{paired ? 'Re-pair' : 'Pair'}</span>
              </button>
            </div>
          </form>
          {paired && (
            <button type="button" className="btn-secondary" onClick={unpair} disabled={unpairing}>
              {unpairing ? <Loader2 size={14} className="animate-spin" /> : <Unlink size={14} />}
              <span>Unpair</span>
            </button>
          )}
          {pairMessage && <Alert kind="success">{pairMessage}</Alert>}
          {pairError && <Alert kind="error">{pairError}</Alert>}
        </div>

        <div className="panel dr-section">
          <div className="panel-header">
            <h3>
              <KeyRound size={16} /> Recovery key by hand
            </h3>
            <Badge tone={keyPinned ? 'success' : 'danger'}>{keyPinned ? 'Pinned' : 'None'}</Badge>
          </div>
          {keyPinned ? (
            <p className="dr-hint">
              The key is pinned{paired ? ' by pairing' : ''}. Rotating it means a new ceremony and a fresh data directory; there is no button
              for that on purpose.
            </p>
          ) : (
            <>
              <p className="dr-hint">
                For a server with no KyRecovery. Run the suite ceremony once, keep the custodian cards, and paste the public key it shows, with
                the split it used. Capsules then go to the local directory.
              </p>
              <form onSubmit={pin} className="dr-stack">
                <label className="dr-field">
                  <span>Suite recovery public key</span>
                  <textarea className="dr-mono" rows={4} value={pinKey} onChange={(e) => setPinKey(e.target.value)} placeholder="base64 from the ceremony page" required />
                </label>
                <div className="dr-row">
                  <label className="dr-field dr-narrow">
                    <span>Needed</span>
                    <input type="number" min={2} max={255} value={pinK} onChange={(e) => setPinK(e.target.value)} required />
                  </label>
                  <label className="dr-field dr-narrow">
                    <span>Of</span>
                    <input type="number" min={2} max={255} value={pinN} onChange={(e) => setPinN(e.target.value)} required />
                  </label>
                  <button type="submit" disabled={pinning}>
                    {pinning ? <Loader2 size={14} className="animate-spin" /> : <KeyRound size={14} />}
                    <span>Pin key</span>
                  </button>
                </div>
              </form>
              {pinError && <Alert kind="error">{pinError}</Alert>}
            </>
          )}
          {hasLocal && (
            <p className="dr-hint">
              <HardDrive size={12} /> Local copies land in {status?.local_dir}; the newest {status?.local_keep} are kept.
            </p>
          )}
        </div>
      </div>
    </div>
  );
};
