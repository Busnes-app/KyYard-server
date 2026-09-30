import { useEffect, useRef, useState } from 'react';
import { ExternalLink, RotateCw, Scaling, Trash2 } from 'lucide-react';
import { Link } from './Link';
import { useCommand } from './ContainerControls';
import { commandLine, NOT_FOUND, workloadRefusal } from './workloadTexts';
import { secureFetch } from '../api';
import { workloadPath } from '../router';
import { canDestroy, canOperate, type DirectCommand, type Workload } from '../tenant';

const WAITING = 'Command sent; waiting for the cluster agent. Do not retry while its outcome is unknown.';

// useClusterCommand posts one workload or pod command and polls it to its outcome, reporting
// through onStatus with the target's name in front; it never resends.
export function useClusterCommand(base: string, label: string, onStatus: (text: string) => void, onSettled?: () => void, notFound = NOT_FOUND) {
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState<DirectCommand | null>(null);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const say = (text: string) => onStatus(text ? `${label} · ${text}` : '');
  const { command, error } = useCommand(base, sent, (data) => { say(commandLine(data)); onSettled?.(); });
  useEffect(() => { if (error) say("Could not read the command result. Check the cluster's Activity tab before trying again."); }, [error]);
  const send = async (body: object) => {
    setBusy(true); say('');
    try {
      const resp = await secureFetch(`${base}/commands`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!alive.current) return;
      if (!resp.ok) { const text = await workloadRefusal(resp, notFound); if (alive.current) say(text); return; }
      const cmd: DirectCommand = await resp.json();
      if (!alive.current) return;
      setSent(cmd); say(cmd.outcome ? commandLine(cmd) : WAITING);
    } catch { if (alive.current) say("Connection lost. The action may have run. Check the cluster's Activity tab before trying again."); }
    finally { if (alive.current) setBusy(false); }
  };
  return { send, say, busy: busy || (command !== null && !command.outcome) };
}

type Props = { base: string; org: string; endpoint: string; workload: Workload; active: boolean; role: string | undefined; capabilities: string[]; scope: string; open?: boolean; onStatus: (text: string) => void; onRefresh?: () => void };

// WorkloadControls is a Deployment's, StatefulSet's or DaemonSet's icon group: Restart and Scale
// for operators, Delete for admins, Open as a link. The agent must report kubernetes.workloads.
export function WorkloadControls({ base, org, endpoint, workload: w, active, role, capabilities, scope, open, onStatus, onRefresh }: Props) {
  const label = `${w.namespace}/${w.name}`;
  const kind = w.kind.toLowerCase();
  const reference = `${w.namespace}/${kind}/${w.name}`;
  const { send, say, busy } = useClusterCommand(base, label, onStatus, onRefresh);
  const capable = capabilities.includes('kubernetes.workloads');
  const operate = capable && canOperate(role), destroy = capable && canDestroy(role);
  if (!operate && !destroy && !open) return null;
  const restart = () => { if (window.confirm(`Restart ${w.kind} ${label}? Its pods are replaced.\n${scope}`)) void send({ action: 'workload.restart', reference }); };
  const scale = () => {
    const typed = window.prompt(`Scale ${w.kind} ${label} to how many replicas (0 to 1000)?\n${scope}`, String(w.desired))?.trim();
    if (typed === undefined) return;
    const n = Number(typed);
    if (typed === '' || !Number.isInteger(n) || n < 0 || n > 1000) { say('Enter a whole number of replicas from 0 to 1000.'); return; }
    void send({ action: 'workload.scale', reference, expects: { replicas: n } });
  };
  const remove = () => {
    const typed = window.prompt(`Delete ${w.kind} ${label} and its pods? This cannot be undone.\n${scope}\n\nType the name ${w.name} to confirm:`);
    if (typed === w.name) void send({ action: 'workload.delete', reference, confirm: w.name });
  };
  const disabled = busy || !active;
  const icon = 16;
  return <div className="ky-container-actions" role="group" aria-label={`Actions for ${label}`}>
    {operate && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Restart ${label}`} title="Restart" disabled={disabled} onClick={restart}><RotateCw size={icon} aria-hidden /></button>}
    {operate && kind !== 'daemonset' && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Scale ${label}`} title="Scale" disabled={disabled} onClick={scale}><Scaling size={icon} aria-hidden /></button>}
    {destroy && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Delete ${label}`} title="Delete" disabled={disabled} onClick={remove}><Trash2 size={icon} aria-hidden /></button>}
    {open && <Link className="btn btn-secondary ky-icon-button" aria-label={`Open ${label}`} title="Open" to={workloadPath(org, endpoint, w.namespace, kind, w.name)}><ExternalLink size={icon} aria-hidden /></Link>}
  </div>;
}
