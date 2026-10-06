import type { Container, DirectCommand } from '../tenant';
import { configurationFailure } from './ContainerConfigurationForm';
import { parseConfiguration, toSpec, unsupportedLabel } from './containerConfiguration';
import { useImageUpdate, type Plan, type Step } from './useImageUpdate';

const LOST = 'Connection lost. The update may have been sent. Check recent activity before trying again.';

export function useContainerImageUpdate(base: string, onSent: (command: DirectCommand, container: Container) => void) {
  const plan = async (container: Container, { signal, current }: Step): Promise<Plan> => {
    const url = `${base}/containers/${encodeURIComponent(container.id)}`;
    const response = await fetch(`${url}/configuration`, { cache: 'no-store', signal });
    if (signal.aborted || !current()) return null;
    if (!response.ok) return { error: await configurationFailure(response) };
    const expects = { image_id: container.image_id, created_unix: Math.floor(Date.parse(container.created_at) / 1000), state: container.state };
    const config = parseConfiguration(await response.json(), { container_id: container.id, ...expects });
    if (signal.aborted || !current()) return null;
    if (!config) return { error: 'The configuration did not match this container. Refresh the inventory.' };
    if (config.unsupported.length) return { error: `Cannot update without losing settings: ${config.unsupported.map(unsupportedLabel).join('; ')}.` };
    const spec = toSpec(config);
    spec.image_id = '';
    spec.image = { reference: container.image, digest: '' };
    return { url: `${url}/recreate`, body: { spec, expects, acknowledge_binds: [], confirm: container.name }, refusal: configurationFailure };
  };
  return useImageUpdate(base, plan, 'container.recreate', LOST, onSent);
}
