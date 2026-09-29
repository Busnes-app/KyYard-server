import { expect, it } from 'vitest';
import { attachments, healthBadge, primaryIP, uptime } from './containerFacts';

it('normalises old string networks and picks the first IP', () => {
  expect(attachments({ networks: ['bridge'] })).toEqual([{ name: 'bridge' }]);
  expect(primaryIP({ networks: ['bridge'] })).toBe('');
  expect(primaryIP({ networks: [{ name: 'a' }, { name: 'b', ip: '172.18.0.3' }, { name: 'c', ip: '10.0.0.9' }] })).toBe('172.18.0.3');
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
  expect(healthBadge('healthy')).toEqual({ text: 'healthy', className: 'badge-success' });
  expect(healthBadge('unhealthy')).toEqual({ text: 'unhealthy', className: 'badge-danger' });
  expect(healthBadge('starting')).toEqual({ text: 'starting', className: 'badge-accent' });
  expect(healthBadge('weird')).toBeNull();
});
