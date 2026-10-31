import React from 'react';

// danger: down, failing, missing. warning: degraded, starting. success: up, passing. None: neutral.
export type Tone = 'danger' | 'warning' | 'success' | undefined;

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

// The ky-ui status pill: a word plus a dot, never the accent. A string is shown in sentence case.
export const Status: React.FC<{ tone?: Tone; title?: string; children: React.ReactNode }> = ({ tone, title, children }) =>
  <span className="ky-status" data-ky-status={tone} title={title}>{typeof children === 'string' ? sentence(children) : children}</span>;
