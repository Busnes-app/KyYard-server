import { useEffect, useRef, useState } from 'react';
import { secureFetch } from '../api';
import type { DirectCommand } from '../tenant';

export interface Step { signal: AbortSignal; current: () => boolean }
// A plan is the refusal text of the read phase, or the write to send; null means the scope ended.
export type Plan = { error: string } | { url: string; body: unknown; refusal: (r: Response) => Promise<string> } | null;

// Owned by the page, so filtering a row or changing tabs cannot unlock a pending update. The caller
// supplies only its read and its write; the lock, scope generation, abort and lost-response
// handling live here. A write whose answer was lost is never resubmitted.
export function useImageUpdate<T>(base: string, plan: (item: T, step: Step) => Promise<Plan>, action: string, lostText: string, onSent: (command: DirectCommand, item: T) => void) {
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

  const update = async (item: T) => {
    if (locked.current) return;
    locked.current = true;
    setBusy(true); setError('');
    const scope = generation.current;
    const current = () => scope === generation.current;
    let submitted = false, uncertain = false;
    const controller = new AbortController();
    read.current = controller;
    try {
      const p = await plan(item, { signal: controller.signal, current });
      if (controller.signal.aborted || !current() || !p) return;
      if ('error' in p) { setError(p.error); return; }
      read.current = null;
      submitted = true;
      const result = await secureFetch(p.url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(p.body) });
      if (!result.ok) { const text = await p.refusal(result); if (current()) setError(text); return; }
      const command: unknown = await result.json();
      if (!command || typeof command !== 'object' || !('id' in command) || typeof command.id !== 'string' || !command.id) throw new Error('Invalid command receipt');
      onSent({ id: command.id, action, outcome: '' }, item);
    } catch {
      uncertain = submitted;
      if (current() && !controller.signal.aborted) {
        setLost(uncertain);
        setError(uncertain ? lostText : 'Could not read the current settings. No update was sent. Try again.');
      }
    } finally {
      if (current()) { locked.current = uncertain; setBusy(false); }
    }
  };
  return { update, busy, error, lost };
}
