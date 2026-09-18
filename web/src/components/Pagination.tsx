import { useState } from 'react';

export function usePagination<T>(rows: T[], scope: string) {
  const [selection, setSelection] = useState({ scope, page: 0 });
  const page = selection.scope === scope ? Math.min(selection.page, Math.max(0, Math.ceil(rows.length / 25) - 1)) : 0;
  if (selection.scope !== scope || selection.page !== page) setSelection({ scope, page });
  return {
    rows: rows.slice(page * 25, (page + 1) * 25),
    controls: rows.length > 25 ? <nav className="ky-pagination" aria-label="Pagination">
      <button className="btn-secondary" disabled={page === 0} onClick={() => setSelection({ scope, page: page - 1 })}>Previous page</button>
      <span aria-live="polite">{page * 25 + 1}–{Math.min((page + 1) * 25, rows.length)} of {rows.length}</span>
      <button className="btn-secondary" disabled={(page + 1) * 25 >= rows.length} onClick={() => setSelection({ scope, page: page + 1 })}>Next page</button>
    </nav> : null,
  };
}
