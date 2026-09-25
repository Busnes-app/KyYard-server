import React from 'react';
import { usePagination } from './Pagination';
import { EmptyNotice } from './StateNotice';

// A titled, paged resource table whose rows become labelled cards at mobile widths.
export function ResourceTable<T>({ title, rows, empty, head, render }: { title: string; rows: T[]; empty: string; head: string[]; render: (r: T) => React.ReactNode[] }) {
  const pagination = usePagination(rows, title);
  return (
    <section className="panel">
      <div className="panel-header"><h2 style={{ fontSize: 16 }}>{title} <span style={{ color: 'var(--ink)', fontWeight: 400 }}>({rows.length})</span></h2></div>
      {pagination.controls}
      {rows.length === 0 ? <EmptyNotice>{empty}</EmptyNotice> : (
        <div style={{ overflowX: 'auto' }}>
          <table className="ky-table ky-responsive-table">
            <thead><tr>{head.map((h) => <th key={h}>{h}</th>)}</tr></thead>
            <tbody>{pagination.rows.map((r, i) => <tr key={i}>{render(r).map((cell, j) => <td key={j} data-label={head[j]}>{cell}</td>)}</tr>)}</tbody>
          </table>
        </div>
      )}
    </section>
  );
}
