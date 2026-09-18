import React, { useState } from 'react';
import { useTenantResource, type AuditRecord } from '../tenant';
import { EmptyNotice, StateNotice } from './StateNotice';

const PAGE = 50;

export const AuditList: React.FC<{ url: string }> = ({ url }) => {
  const [offset, setOffset] = useState(0);
  const { state, data, reload } = useTenantResource<AuditRecord[]>(`${url}?offset=${offset}&limit=${PAGE}`);
  return (
    <section className="panel" aria-labelledby="audit-heading">
      <div className="panel-header"><h2 id="audit-heading" style={{ fontSize: 16 }}>Audit history</h2></div>
      <StateNotice state={state} onRetry={reload} />
      {state === 'ready' && data && (data.length === 0 ? <EmptyNotice>No recorded activity on this page.</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Result</th><th>Request</th></tr></thead>
            <tbody>
              {data.map((r) => (
                <tr key={r.id}>
                  <td data-label="Time"><time dateTime={r.created_at}>{new Date(r.created_at).toLocaleString()}</time></td>
                  <td data-label="Actor" className="font-mono">{r.user_id}</td>
                  <td data-label="Action" className="font-mono">{r.action}</td>
                  <td data-label="Target" className="font-mono">{r.resource}</td>
                  <td data-label="Result"><span className={`badge ${r.result === 'success' ? 'badge-success' : r.result === 'denied' || r.result === 'failure' ? 'badge-danger' : ''}`}>{r.result}</span></td>
                  <td data-label="Request" className="font-mono" style={{ fontSize: 11 }}>{r.correlation_id}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
        <button className="btn-secondary" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>Newer</button>
        <button className="btn-secondary" disabled={state !== 'ready' || !data || data.length < PAGE} onClick={() => setOffset(offset + PAGE)}>Older</button>
      </div>
    </section>
  );
};
