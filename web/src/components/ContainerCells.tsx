import React from 'react';
import { displayName } from './Endpoints';
import { attachments, healthBadge, primaryIP, stateBadge } from './containerFacts';
import type { Container, Port } from '../tenant';

// merged shows one chip: a running container's health when it has a check, else its state.
export const StateCell: React.FC<{ c: Container; merged?: boolean }> = ({ c, merged = false }) => {
  const health = healthBadge(c.health);
  if (merged && health && c.state === 'running') return <span className={`badge ${health.className}`} title={c.status}>{health.text}</span>;
  return <span className="ky-state-cell"><span className={stateBadge(c.state)} title={c.status}>{displayName(c.state)}</span>{health && <span className={`badge ${health.className}`}>{health.text}</span>}</span>;
};

export const IPCell: React.FC<{ c: Container }> = ({ c }) => <span title={attachments(c).map((n) => `${n.name}: ${n.ip || '—'}`).join('\n')}>{primaryIP(c) || '—'}</span>;

// One line for a table row: the first published mapping and how many more; the drawer lists all.
export const PortSummary: React.FC<{ ports: Port[] }> = ({ ports }) => {
  const mappings = [...new Set(ports.map((p) => p.host ? `${p.host} → ${p.container}/${p.protocol}` : `${p.container}/${p.protocol}`))];
  if (!mappings.length) return <span className="ky-muted">—</span>;
  return <span title={mappings.join('\n')}>{mappings[0]}{mappings.length > 1 && <span className="ky-muted"> +{mappings.length - 1}</span>}</span>;
};
