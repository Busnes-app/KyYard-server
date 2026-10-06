import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
import type { DirectCommand, Workload } from '../tenant';
import { parseWorkloadConfiguration, toWorkloadSpec, tracksTag, workloadUnsupportedLabel } from './workloadConfiguration';
import { workloadURL } from './WorkloadConfigurationForm';
import { workloadRefusal } from './workloadTexts';

// Owned by the page, so filtering a row or changing tabs cannot unlock a pending update.
export function useWorkloadImageUpdate(base: string, onSent: (command: DirectCommand, workload: Workload) => void) {
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

  const update = async (w: Workload) => {
    if (locked.current) return;
    locked.current = true;
    setBusy(true); setError('');
    const scope = generation.current;
    const current = () => scope === generation.current;
    let submitted = false, uncertain = false;
    const controller = new AbortController();
    read.current = controller;
    const target = { namespace: w.namespace, kind: w.kind.toLowerCase(), name: w.name };
    try {
      const url = workloadURL(base, target);
      const response = await fetch(`${url}/configuration`, { cache: 'no-store', signal: controller.signal });
      if (controller.signal.aborted || !current()) return;
      if (!response.ok) { setError(await workloadRefusal(response)); return; }
      const config = parseWorkloadConfiguration(await response.json(), target);
      if (controller.signal.aborted || !current()) return;
      if (!config) { setError('The configuration did not match this workload. Refresh the inventory.'); return; }
      if (config.unsupported.length) { setError(`Cannot update without losing settings: ${config.unsupported.map(workloadUnsupportedLabel).join('; ')}.`); return; }
      const spec = toWorkloadSpec(config);
      const pull = [...spec.containers, ...spec.init_containers].filter((c) => tracksTag(c.image)).map((c) => c.name);
      if (!pull.length) { setError('Every image is pinned by digest only; there is no tag to update from.'); return; }
      read.current = null;
      submitted = true;
      const result = await secureFetch(`${url}/apply`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resource_version: spec.resource_version, spec, confirm: w.name, pull }) });
      if (!result.ok) { if (current()) setError(await workloadRefusal(result)); return; }
      const command: unknown = await result.json();
      if (!command || typeof command !== 'object' || !('id' in command) || typeof command.id !== 'string' || !command.id) throw new Error('Invalid command receipt');
      onSent({ id: command.id, action: 'workload.apply', outcome: '' }, w);
    } catch {
      uncertain = submitted;
      if (current() && !controller.signal.aborted) {
        setLost(uncertain);
        setError(uncertain ? "Connection lost. The update may have been sent. Check the workload's Activity tab before trying again." : 'Could not read the current settings. No update was sent. Try again.');
      }
    } finally {
      if (current()) { locked.current = uncertain; setBusy(false); }
    }
  };
  return { update, busy, error, lost };
}
