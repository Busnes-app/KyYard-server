import { useState } from 'react';
import { secureFetch } from '../api';

export function ApplicationRevisionEditor({ base, name, expected, onSaved }: { base: string; name: string; expected: number; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [blocked, setBlocked] = useState(false);
  return <section className="dr-stack">
    <button type="button" className="btn-secondary" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? 'Cancel revision edit' : 'Save new revision'}</button>
    {open && <RevisionForm base={base} name={name} expected={expected} onSaved={onSaved} blocked={blocked} onBlocked={() => setBlocked(true)} />}
  </section>;
}
function RevisionForm({ base, name, expected, onSaved, blocked, onBlocked }: { base: string; name: string; expected: number; onSaved: () => void; blocked: boolean; onBlocked: () => void }) {
  const [source, setSource] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const save = async () => {
    if (!window.confirm(`Save a new definition for ${name} based on revision ${expected}? All services and environment values must be supplied. Running containers will not change.`)) return;
    setBusy(true); setMessage('');
    try {
      const response = await secureFetch(`${base}/revisions`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expected_revision: expected, compose: source }) });
      if (response.ok) { setSource(''); onSaved(); return; }
      if (response.status === 400) setMessage('Definition refused. Check the supported Compose fields, explicit values and 64 KiB limit.');
      else if (response.status === 403) setMessage('You do not have permission to edit this application.');
      else { onBlocked(); setMessage(response.status === 409 ? 'The revision changed or the history limit was reached. Refresh applications and review the saved definition before editing again.' : 'The save outcome is unknown or the application is unavailable. Refresh applications and check its revision before editing again.'); }
    } catch { onBlocked(); setMessage('The save outcome is unknown. Refresh applications and check its revision before editing again.'); }
    finally { setBusy(false); }
  };
  return <form className="dr-stack" onSubmit={(event) => { event.preventDefault(); void save(); }}>
    <p>Replace the complete definition for {name}, based on revision {expected}. Supply the same supported Compose fields as import, including every environment value. Values omitted here are not copied from earlier revisions. Saving does not deploy or change adopted containers.</p>
    <label>Replacement Compose YAML (up to 64 KiB)<textarea value={source} onChange={(event) => setSource(event.target.value)} rows={10} maxLength={65536} autoComplete="off" spellCheck={false} required disabled={busy || blocked} /></label>
    <button type="submit" disabled={busy || blocked || !source.trim()}>Save revision {expected + 1}</button>
    <button type="button" className="btn-secondary" disabled={busy} onClick={() => setSource('')}>Clear replacement input</button>
    {(message || blocked) && <p role="status">{message || 'Refresh applications and check its revision before editing again.'}</p>}
  </form>;
}
