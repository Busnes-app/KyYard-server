import React, { useState } from 'react';
import { ChevronRight } from 'lucide-react';
import { usePagination } from './Pagination';
import { EmptyNotice } from './StateNotice';

// A titled, paged resource table whose rows become labelled cards at mobile widths. rowKey keeps a
// row's state (an open terminal or log viewer) when a refresh adds or removes rows ahead of it.
// detail adds a disclosure per row that opens a full-width drawer under it; it needs rowKey.
export function ResourceTable<T>({ title, rows, empty, head, render, rowKey, detail, compact = false }: { title: string; rows: T[]; empty: string; head: string[]; render: (r: T) => React.ReactNode[]; rowKey?: (r: T) => string; detail?: { label: (r: T) => string; render: (r: T) => React.ReactNode }; compact?: boolean }) {
  const pagination = usePagination(rows, title);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const toggle = (key: string) => setOpen((old) => { const next = new Set(old); if (!next.delete(key)) next.add(key); return next; });
  return (
    <section className="panel ky-table-panel">
      <div className="panel-header"><h2 className="ky-table-title">{title} <span>({rows.length})</span></h2></div>
      {pagination.controls}
      {rows.length === 0 ? <EmptyNotice>{empty}</EmptyNotice> : (
        <div className="ky-table-scroll">
          <table className={`ky-table ky-responsive-table${compact ? ' ky-table-compact' : ''}`}>
            <thead><tr>{detail && <th className="ky-disclosure-cell"><span className="sr-only">Details</span></th>}{head.map((h) => <th key={h}>{h === 'Actions' && compact ? <span className="sr-only">{h}</span> : h}</th>)}</tr></thead>
            <tbody>{pagination.rows.map((r, i) => {
              const key = rowKey ? rowKey(r) : String(i);
              const expanded = !!detail && open.has(key);
              const drawer = `drawer-${key}`.replace(/[^A-Za-z0-9_-]/g, '_');
              return <React.Fragment key={key}>
                <tr className={expanded ? 'ky-row-open' : undefined}>
                  {detail && <td className="ky-disclosure-cell"><button type="button" className="ky-disclosure" aria-expanded={expanded} aria-controls={expanded ? drawer : undefined} aria-label={`Show details for ${detail.label(r)}`} onClick={() => toggle(key)}><ChevronRight size={16} aria-hidden /></button></td>}
                  {render(r).map((cell, j) => <td key={j} data-label={head[j]} className={head[j] === 'Actions' && compact ? 'ky-row-actions' : undefined}>{cell}</td>)}
                </tr>
                {expanded && <tr className="ky-detail-row"><td id={drawer} colSpan={head.length + 1}>{detail.render(r)}</td></tr>}
              </React.Fragment>;
            })}</tbody>
          </table>
        </div>
      )}
    </section>
  );
}
