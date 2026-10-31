import { expect, it } from 'vitest';
import { attachments, containerStatus, healthBadge, needsAttention, primaryIP, uptime } from './containerFacts';

it('falls back to network names and picks the first IP', () => {
  expect(attachments({ networks: ['bridge'] })).toEqual([{ name: 'bridge' }]);
  expect(attachments({} as { networks: string[] })).toEqual([]);
  expect(primaryIP({ networks: ['bridge'] })).toBe('');
  expect(primaryIP({ networks: ['a', 'b', 'c'], network_attachments: [{ name: 'a' }, { name: 'b', ip: '172.18.0.3' }, { name: 'c', ip: '10.0.0.9' }] })).toBe('172.18.0.3');
});

it('formats uptime from started_at and says nothing when unknown', () => {
  const now = Date.parse('2026-09-29T12:00:00Z');
  expect(uptime(undefined, now)).toBe('');
  expect(uptime('', now)).toBe('');
  expect(uptime('0001-01-01T00:00:00Z', now)).toBe('');
  expect(uptime('2026-09-29T11:59:15Z', now)).toBe('45s');
  expect(uptime('2026-09-29T09:45:00Z', now)).toBe('2h 15m');
  expect(uptime('2026-09-26T08:00:00Z', now)).toBe('3d 4h');
  expect(uptime('2026-09-29T12:00:30Z', now)).toBe('0s');
});

it('maps health to a badge and hides unknown health', () => {
  expect(healthBadge(undefined)).toBeNull();
  expect(healthBadge('')).toBeNull();
  expect(healthBadge('none')).toBeNull();
  expect(healthBadge('healthy')).toEqual({ text: 'healthy', tone: 'success' });
  expect(healthBadge('unhealthy')).toEqual({ text: 'unhealthy', tone: 'danger' });
  expect(healthBadge('starting')).toEqual({ text: 'starting', tone: 'warning' });
  expect(healthBadge('weird')).toBeNull();
  expect(healthBadge('toString')).toBeNull();
  expect(healthBadge('__proto__')).toBeNull();
});

it('puts failing and starting containers first, never a created or healthy one', () => {
  expect(containerStatus({ state: 'running', health: 'unhealthy' })).toEqual({ text: 'unhealthy', tone: 'danger' });
  expect(containerStatus({ state: 'exited', health: 'healthy' })).toEqual({ text: 'exited', tone: 'danger' });
  expect(containerStatus({ state: 'created' })).toEqual({ text: 'created', tone: undefined });
  expect([
    { state: 'running', health: 'unhealthy' }, { state: 'exited' }, { state: 'dead' }, { state: 'restarting' }, { state: 'running', health: 'starting' },
  ].every(needsAttention)).toBe(true);
  expect([{ state: 'running' }, { state: 'running', health: 'healthy' }, { state: 'created' }, { state: 'paused' }].some(needsAttention)).toBe(false);
});
