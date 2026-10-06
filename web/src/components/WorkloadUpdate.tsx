import { useCallback } from 'react';
import type { Pod, Workload } from '../tenant';
import { UpdateBadge, useUpdateQueue, type Verdict } from './ContainerUpdate';

export type CheckWorkload = (workload: Workload, pods: Pod[], signal: AbortSignal, fresh: boolean) => Promise<Verdict | null>;
const ref = (w: Workload) => `${w.namespace}/${w.kind.toLowerCase()}/${w.name}`;
// The images the pods run are the workload's identity: a rollout invalidates the cached answer.
const identity = (w: Workload, pods: Pod[]) => JSON.stringify([ref(w), w.images, pods.map((p) => [p.name, p.containers.map((c) => [c.image, c.image_id])])]);

export function useWorkloadUpdateChecker(base: string): CheckWorkload {
  const run = useUpdateQueue();
  return useCallback((w, pods, signal, fresh) => run(
    JSON.stringify([base, identity(w, pods)]),
    `${base}/workloads/${encodeURIComponent(w.namespace)}/${encodeURIComponent(w.kind.toLowerCase())}/${encodeURIComponent(w.name)}/updates/check`,
    (body) => 'workload' in body && body.workload === ref(w), signal, fresh,
  ), [base, run]);
}

export function WorkloadUpdate({ workload, pods, active, org, checkUpdate, onUpdate, updateDisabled = false }: { workload: Workload; pods: Pod[]; active: boolean; org: string; checkUpdate: CheckWorkload; onUpdate: () => void; updateDisabled?: boolean }) {
  return <UpdateBadge label={`${workload.namespace}/${workload.name}`} identity={identity(workload, pods)} active={active} org={org}
    check={(signal, fresh) => checkUpdate(workload, pods, signal, fresh)} onUpdate={onUpdate} updateDisabled={updateDisabled} />;
}
