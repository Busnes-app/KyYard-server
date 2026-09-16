import React from 'react';
import type { LoadState } from '../tenant';

const messages: Record<Exclude<LoadState, 'ready'>, string> = {
  loading: 'Loading…',
  denied: 'You do not have access to this organization. Ask an organization administrator for membership.',
  notfound: 'Nothing here. It may have been removed, or the link is wrong.',
  offline: 'Offline: the server could not be reached. Check your connection and retry.',
  error: 'Something went wrong on the server. Retry, or check the server log.',
};

export const StateNotice: React.FC<{ state: LoadState; onRetry?: () => void }> = ({ state, onRetry }) => {
  if (state === 'ready') return null;
  const role = state === 'loading' ? 'status' : 'alert';
  return (
    <div className="panel" role={role} style={{ display: 'flex', gap: 12, alignItems: 'center', justifyContent: 'space-between' }}>
      <span>{messages[state]}</span>
      {onRetry && state !== 'loading' && state !== 'denied' && <button className="btn-secondary" onClick={onRetry}>Retry</button>}
    </div>
  );
};

export const EmptyNotice: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <p style={{ color: 'var(--ink)', padding: '12px 0' }}>{children}</p>
);
