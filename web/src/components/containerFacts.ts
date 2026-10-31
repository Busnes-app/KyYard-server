import { useEffect, useState } from 'react';
import type { Container, NetworkAttachment } from '../tenant';
import type { Tone } from './Status';

type Nets = Pick<Container, 'networks' | 'network_attachments'>;
// Older agents send names only, and the oldest reports may lack networks entirely.
export const attachments = (c: Nets): NetworkAttachment[] => c.network_attachments ?? (c.networks ?? []).map((name) => ({ name }));
export const primaryIP = (c: Nets): string => attachments(c).find((n) => n.ip)?.ip ?? '';

// A zero time (year 1) is the agent saying "unknown"; it is never an uptime.
export const uptime = (startedAt: string | undefined, now: number): string => {
  if (!startedAt) return '';
  const started = Date.parse(startedAt);
  if (!Number.isFinite(started) || new Date(started).getUTCFullYear() < 1970) return '';
  const s = Math.max(0, Math.floor((now - started) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60), h = Math.floor(m / 60), d = Math.floor(h / 24);
  if (h < 1) return `${m}m`;
  if (d < 1) return `${h}h ${m % 60}m`;
  return `${d}d ${h % 24}h`;
};

const healthTones: Record<string, Tone> = { healthy: 'success', unhealthy: 'danger', starting: 'warning' };
export const healthBadge = (health: string | undefined): { text: string; tone: Tone } | null => health && Object.hasOwn(healthTones, health) ? { text: health, tone: healthTones[health] } : null;

export const stateTone = (state: string): Tone => state === 'running' ? 'success' : state === 'exited' || state === 'dead' ? 'danger' : state === 'restarting' ? 'warning' : undefined;

// One status per container: a running container's health when it has a check, else its state.
// A danger or warning tone is what triage puts first.
export const containerStatus = (c: Pick<Container, 'state' | 'health'>): { text: string; tone: Tone } => {
  const health = healthBadge(c.health);
  return health && c.state === 'running' ? health : { text: c.state, tone: stateTone(c.state) };
};
export const needsAttention = (c: Pick<Container, 'state' | 'health'>): boolean => { const { tone } = containerStatus(c); return tone === 'danger' || tone === 'warning'; };

export const bytes = (n: number): string => n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GiB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(0)} MiB` : `${n} B`;
export const ago = (iso: string, now = Date.now()): string => { const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000)); return s < 90 ? `${s}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`; };

// Ticks once a second while mounted so uptime is live; the value is the clock, not the row.
export function useNow(): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
  return now;
}
