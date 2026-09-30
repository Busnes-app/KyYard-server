import { refusal } from '../tenant';

// Fixed texts for cluster workload and pod actions. Server and agent text never renders.
export const UPGRADE_CLUSTER = 'Upgrade the cluster agent and re-apply the manifest to enable this.';
export const NOT_GRANTED = 'This namespace is not granted to KyYard on this cluster.';
export const NOT_FOUND = 'The workload is no longer in the cluster.';
export const CONFLICT = 'The workload changed since you read it. Read again.';
export const FORBIDDEN = "The agent's role does not allow this. Regenerate and apply the cluster manifest, then retry.";
export const UNREPORTED = 'This workload has settings the form cannot carry; nothing was changed.';
// The inventory names an application only on a Deployment KyYard applied; elsewhere it is unknown.
export const POD_SECURITY = 'This namespace does not enforce Pod Security baseline or restricted; KyYard refuses to change workloads there.';
export const MANAGED = 'Managed by a KyYard application. Edit it there.';
export const PVC_RETENTION = "Scaling down would delete this StatefulSet's volume claims (whenScaled: Delete).";
export const NAME_TAKEN = 'A workload with this name already exists in the namespace. Choose another name.';
// A run's 404 is the endpoint or its inventory; the server's body does not say which.
export const RUN_NOT_FOUND = 'The endpoint or its inventory was not found.';

// Step codes of a workload apply whose wording differs from an application deployment's.
export const WORKLOAD_STEPS: Record<string, string> = {
  forbidden: FORBIDDEN, conflict: CONFLICT, not_found: NOT_FOUND, namespace_not_granted: NOT_GRANTED,
  configuration_unreported: UNREPORTED, application_managed: MANAGED, pod_security: POD_SECURITY, pvc_retention: PVC_RETENTION, name_taken: NAME_TAKEN,
};
// Detail words of a settled workload or pod command (the cluster agent's Operate).
const DETAILS: Record<string, string> = {
  ...WORKLOAD_STEPS,
  admission_denied: 'The cluster refused the change (quota or policy).',
  unsupported: 'The agent does not run this action on this kind.',
  replicas_out_of_range: 'Replicas must be from 0 to 1000.',
  invalid_reference: 'The agent refused the workload reference.',
  runtime_timeout: 'The cluster did not answer in time.',
  runtime_error: 'The cluster call failed.',
};
export const POD_NOT_FOUND = 'The pod is no longer in the cluster.';
// Detail words that differ by action.
const ACTION_DETAILS: Record<string, Record<string, string>> = {
  'pod.delete': {
    not_found: POD_NOT_FOUND,
    conflict: 'The pod was replaced since the inventory was read. Refresh and try again.',
  },
  'workload.delete': {
    pvc_retention: 'Deleting this StatefulSet would delete its volume claims (whenDeleted: Delete). Set whenDeleted: Retain first.',
  },
};
function detailText(detail = '', action = ''): string {
  const status = /^runtime_status ([1-5][0-9]{2})$/.exec(detail);
  if (status) return `The cluster refused with status ${status[1]}.`;
  const own = Object.hasOwn(ACTION_DETAILS, action) ? ACTION_DETAILS[action] : undefined;
  if (own && Object.hasOwn(own, detail)) return own[detail] ?? '';
  return Object.hasOwn(DETAILS, detail) ? DETAILS[detail] ?? '' : '';
}

export const ACTIONS: Record<string, string> = { 'workload.restart': 'Restart', 'workload.scale': 'Scale', 'workload.delete': 'Delete', 'pod.delete': 'Delete', 'workload.apply': 'Apply', 'workload.run': 'Run' };
export const OUTCOMES: Record<string, string> = { '': 'pending', succeeded: 'done', failed: 'failed', denied: 'refused', unknown: 'outcome unknown; check the cluster before trying again', timed_out: "did not answer in time; check the cluster's Activity tab before trying again" };
// commandLine is a command as one fixed sentence; action, outcome and detail are never shown raw.
export function commandLine(c: { action: string; outcome: string; detail?: string }): string {
  const action = Object.hasOwn(ACTIONS, c.action) ? ACTIONS[c.action] : 'Command';
  const outcome = Object.hasOwn(OUTCOMES, c.outcome) ? OUTCOMES[c.outcome] : 'unrecognised outcome';
  const detail = detailText(c.detail, c.action);
  return `${action} ${outcome}.${detail ? ` ${detail}` : ''}`;
}

const CONFLICTS: Record<string, string> = {
  application_managed: MANAGED,
  command_in_progress: 'A workload apply or run, or a container change, on this cluster is waiting for its result.',
  runtime_unsupported: "This endpoint's runtime does not support this action.",
  endpoint_offline: 'The cluster agent is not connected. Reconnect it and try again.',
  deployment_not_sent: 'The command was not sent to the cluster agent. Refresh and try again.',
};
// workloadRefusal maps a refused workload or pod request to a fixed text.
export async function workloadRefusal(resp: Response, notFound = NOT_FOUND): Promise<string> {
  if (resp.status === 501) return UPGRADE_CLUSTER;
  if (resp.status === 429) return 'Too many requests. Wait a minute and try again.';
  if (resp.status === 422) {
    const body: unknown = await resp.json().catch(() => null);
    const code = body && typeof body === 'object' && 'code' in body ? body.code : '';
    if (code === 'namespace_not_granted') return NOT_GRANTED;
    const blockers = body && typeof body === 'object' && 'blockers' in body && Array.isArray(body.blockers) ? body.blockers.filter((b): b is string => typeof b === 'string') : [];
    return blockers.length ? [...new Set(blockers.map(blockerText))].join(' ') : 'The server refused part of this configuration.';
  }
  return refusal(resp, {
    forbidden: 'You do not have permission for this action.',
    invalid: 'The server refused the request. Check the typed name and refresh the inventory.',
    notFound, conflict: CONFLICTS,
  });
}
// spec_invalid:<field> names the WorkloadConfiguration field the server's validation refused.
const FIELDS: Record<string, string> = {
  replicas: 'replicas', paused: 'paused', strategy: 'update strategy', containers: 'containers', container_name: 'container name',
  image: 'image', argv: 'command or arguments', env: 'environment', env_from: 'imported environment', resources: 'resources',
  resource_version: 'version', unsupported: 'unsupported settings', frame: 'size', size: 'size',
};
function blockerText(code: string): string {
  if (code === 'configuration_incomplete') return UNREPORTED;
  if (code === 'name_taken') return NAME_TAKEN;
  const field = code.startsWith('spec_invalid:') ? code.slice('spec_invalid:'.length) : '';
  return Object.hasOwn(FIELDS, field) ? `The server refused the ${FIELDS[field]} setting.` : 'The server refused part of this configuration.';
}
