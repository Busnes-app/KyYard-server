import { ScrollText, Terminal, Trash2 } from 'lucide-react';
import { Link } from './Link';
import { useClusterCommand } from './WorkloadControls';
import { POD_NOT_FOUND } from './workloadTexts';
import { workloadPath } from '../router';
import { canDestroy, canExec, type Pod } from '../tenant';

// Kinds with a workload page, as the route names them.
export const PAGE_KINDS: Record<string, string> = { Deployment: 'deployment', StatefulSet: 'statefulset', DaemonSet: 'daemonset' };

type Props = { base: string; org: string; endpoint: string; pod: Pod; active: boolean; role: string | undefined; capabilities: string[]; scope: string; onStatus: (text: string) => void; onRefresh?: () => void; onLogs?: () => void };

// PodControls deletes a pod by its typed name and links to its workload page's Logs and Terminal
// tabs; a pod no configurable workload owns has no page, so onLogs (the cluster's dialog) serves it.
export function PodControls({ base, org, endpoint, pod, active, role, capabilities, scope, onStatus, onRefresh, onLogs }: Props) {
  const label = `${pod.namespace}/${pod.name}`;
  const { send, busy } = useClusterCommand(base, label, onStatus, onRefresh, POD_NOT_FOUND);
  const destroy = capabilities.includes('kubernetes.workloads') && canDestroy(role);
  const terminal = capabilities.includes('pod.exec') && canExec(role);
  const kind = Object.hasOwn(PAGE_KINDS, pod.owner_kind) ? PAGE_KINDS[pod.owner_kind] ?? '' : '';
  const dialog = !kind && onLogs && pod.containers.length > 0;
  if (!destroy && !kind && !dialog) return null;
  const page = (tab: string) => `${workloadPath(org, endpoint, pod.namespace, kind, pod.owner_name, tab)}&pod=${encodeURIComponent(pod.name)}`;
  const remove = () => {
    const typed = window.prompt(`Delete pod ${label}? Its controller may start a replacement.\n${scope}\n\nType the pod name ${pod.name} to confirm:`);
    if (typed === pod.name) void send({ action: 'pod.delete', reference: `${pod.namespace}/pod/${pod.name}`, confirm: pod.name });
  };
  const icon = 16;
  return <div className="ky-container-actions" role="group" aria-label={`Actions for ${label}`}>
    {kind && <Link className="btn btn-secondary ky-icon-button" aria-label={`Logs for ${label}`} title="Logs" to={page('logs')}><ScrollText size={icon} aria-hidden /></Link>}
    {dialog && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Logs for ${label}`} title="Logs" disabled={!active} onClick={onLogs}><ScrollText size={icon} aria-hidden /></button>}
    {kind && terminal && <Link className="btn btn-secondary ky-icon-button" aria-label={`Terminal for ${label}`} title="Terminal" to={page('terminal')}><Terminal size={icon} aria-hidden /></Link>}
    {destroy && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Delete ${label}`} title="Delete" disabled={busy || !active} onClick={remove}><Trash2 size={icon} aria-hidden /></button>}
  </div>;
}
