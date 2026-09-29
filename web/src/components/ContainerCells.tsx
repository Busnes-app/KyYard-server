import React from 'react';
import { displayName } from './Endpoints';
import { attachments, healthBadge, primaryIP, stateBadge } from './containerFacts';
import type { Container } from '../tenant';

export const StateCell: React.FC<{ c: Container }> = ({ c }) => {
  const health = healthBadge(c.health);
  return <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}><span className={stateBadge(c.state)} title={c.status}>{displayName(c.state)}</span>{health && <span className={`badge ${health.className}`}>{health.text}</span>}</span>;
};

export const IPCell: React.FC<{ c: Container }> = ({ c }) => <span title={attachments(c).map((n) => `${n.name}: ${n.ip || '—'}`).join('\n')}>{primaryIP(c) || '—'}</span>;
