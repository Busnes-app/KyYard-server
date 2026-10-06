import type { DirectCommand, Workload } from '../tenant';
import { parseWorkloadConfiguration, toWorkloadSpec, tracksTag, workloadUnsupportedLabel } from './workloadConfiguration';
import { workloadURL } from './WorkloadConfigurationForm';
import { workloadRefusal } from './workloadTexts';
import { useImageUpdate, type Plan, type Step } from './useImageUpdate';

const LOST = "Connection lost. The update may have been sent. Check the workload's Activity tab before trying again.";

export function useWorkloadImageUpdate(base: string, onSent: (command: DirectCommand, workload: Workload) => void) {
  const plan = async (w: Workload, { signal, current }: Step): Promise<Plan> => {
    const target = { namespace: w.namespace, kind: w.kind.toLowerCase(), name: w.name };
    const url = workloadURL(base, target);
    const response = await fetch(`${url}/configuration`, { cache: 'no-store', signal });
    if (signal.aborted || !current()) return null;
    if (!response.ok) return { error: await workloadRefusal(response) };
    const config = parseWorkloadConfiguration(await response.json(), target);
    if (signal.aborted || !current()) return null;
    if (!config) return { error: 'The configuration did not match this workload. Refresh the inventory.' };
    if (config.unsupported.length) return { error: `Cannot update without losing settings: ${config.unsupported.map(workloadUnsupportedLabel).join('; ')}.` };
    const spec = toWorkloadSpec(config);
    const pull = [...spec.containers, ...spec.init_containers].filter((c) => tracksTag(c.image)).map((c) => c.name);
    if (!pull.length) return { error: 'Every image is pinned by digest only; there is no tag to update from.' };
    return { url: `${url}/apply`, body: { resource_version: spec.resource_version, spec, confirm: w.name, pull }, refusal: (r) => workloadRefusal(r) };
  };
  return useImageUpdate(base, plan, 'workload.apply', LOST, onSent);
}
