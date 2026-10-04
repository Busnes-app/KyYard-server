import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
import type { Container, DirectCommand } from '../tenant';
import { configurationFailure } from './ContainerConfigurationForm';
import { parseConfiguration, toSpec, unsupportedLabel } from './containerConfiguration';

// Owned by the page, so filtering a row or changing tabs cannot unlock a pending update.
export function useContainerImageUpdate(base: string, onSent: (command: DirectCommand, container: Container) => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [lost, setLost] = useState(false);
  const locked = useRef(false);
  const read = useRef<AbortController | null>(null);
  const generation = useRef(0);
  useEffect(() => {
    generation.current++;
    locked.current = false; setBusy(false); setLost(false); setError('');
    return () => { generation.current++; read.current?.abort(); };
  }, [base]);

  const update = async (container: Container) => {
    if (locked.current) return;
    locked.current = true;
    setBusy(true); setError('');
    const scope = generation.current;
    const current = () => scope === generation.current;
    let submitted = false, uncertain = false;
    const controller = new AbortController();
    read.current = controller;
    try {
      const url = `${base}/containers/${encodeURIComponent(container.id)}`;
      const response = await fetch(`${url}/configuration`, { cache: 'no-store', signal: controller.signal });
      if (controller.signal.aborted || !current()) return;
      if (!response.ok) { setError(await configurationFailure(response)); return; }
      const expects = { image_id: container.image_id, created_unix: Math.floor(Date.parse(container.created_at) / 1000), state: container.state };
      const config = parseConfiguration(await response.json(), { container_id: container.id, ...expects });
      if (controller.signal.aborted || !current()) return;
      if (!config) { setError('The configuration did not match this container. Refresh the inventory.'); return; }
      if (config.unsupported.length) {
        setError(`Cannot update without losing settings: ${config.unsupported.map(unsupportedLabel).join('; ')}.`);
        return;
      }
      const spec = toSpec(config);
      spec.image_id = '';
      spec.image = { reference: container.image, digest: '' };
      read.current = null;
      submitted = true;
      const result = await secureFetch(`${url}/recreate`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ spec, expects, acknowledge_binds: [], confirm: container.name }),
      });
      if (!result.ok) { if (current()) setError(await configurationFailure(result)); return; }
      const command: unknown = await result.json();
      if (!command || typeof command !== 'object' || !('id' in command) || typeof command.id !== 'string' || !command.id) throw new Error('Invalid command receipt');
      onSent({ id: command.id, action: 'container.recreate', outcome: '' }, container);
    } catch {
      uncertain = submitted;
      if (current() && !controller.signal.aborted) {
        setLost(uncertain);
        setError(uncertain ? 'Connection lost. The update may have been sent. Check recent activity before trying again.' : 'Could not read the current settings. No update was sent. Try again.');
      }
    } finally {
      if (current()) { locked.current = uncertain; setBusy(false); }
    }
  };
  return { update, busy, error, lost };
}
