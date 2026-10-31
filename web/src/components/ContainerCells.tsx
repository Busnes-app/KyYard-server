import React from 'react';
import { displayName } from './Endpoints';
import { attachments, containerStatus, primaryIP } from './containerFacts';
import { Status } from './Status';
import type { Container, Port } from '../tenant';

export const StateCell: React.FC<{ c: Container }> = ({ c }) => {
  const { text, tone } = containerStatus(c);
  return <Status tone={tone} title={c.status}>{displayName(text)}</Status>;
};

export const IPCell: React.FC<{ c: Container }> = ({ c }) => <span title={attachments(c).map((n) => `${n.name}: ${n.ip || '—'}`).join('\n')}>{primaryIP(c) || '—'}</span>;

// One line for a table row: the first published mapping and how many more; the drawer lists all.
export const PortSummary: React.FC<{ ports: Port[] }> = ({ ports }) => {
  const mappings = [...new Set(ports.map((p) => p.host ? `${p.host} → ${p.container}/${p.protocol}` : `${p.container}/${p.protocol}`))];
  if (!mappings.length) return <span className="ky-muted">—</span>;
  return <span title={mappings.join('\n')}>{mappings[0]}{mappings.length > 1 && <span className="ky-muted"> +{mappings.length - 1}</span>}</span>;
};
