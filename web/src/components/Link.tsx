import React from 'react';
import { navigate } from '../router';

// A real anchor: middle-click, copy link and screen readers work; plain clicks stay in-app.
export const Link: React.FC<React.AnchorHTMLAttributes<HTMLAnchorElement> & { to: string; current?: boolean }> = ({ to, current, children, onClick, ...rest }) => (
  <a
    href={to}
    aria-current={current ? 'page' : undefined}
    onClick={(e) => {
      onClick?.(e);
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      navigate(to);
    }}
    {...rest}
  >
    {children}
  </a>
);
