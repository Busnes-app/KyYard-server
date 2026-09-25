import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ComposeProjects } from './ComposeProjects';
import type { Container } from '../tenant';

afterEach(cleanup);

it('keeps exact project identities distinct and renders names as text', () => {
  const prefix = 'a'.repeat(80);
  const names = [`${prefix}-one`, `${prefix}-two`, '<img src=x onerror=alert(1)>'];
  const containers: Container[] = names.map((name, i) => ({ id: String(i), name: `container-${i}`, image: 'alpine:3', image_id: 'i', state: 'running', status: '', created_at: '', ports: [], networks: [], labels: { password: 'never-show-labels' }, compose_project: name }));
  const onSelect = vi.fn();
  const { container } = render(<ComposeProjects containers={containers} truncated={false} onSelect={onSelect} />);
  expect(screen.getByRole('heading', { name: 'Compose projects (3)' })).toBeTruthy();
  for (const name of names) fireEvent.click(screen.getByTitle(name));
  const buttons = screen.getAllByRole('button');
  for (const button of buttons) fireEvent.click(button);
  expect(new Set(onSelect.mock.calls.map(([name]) => name))).toEqual(new Set(names));
  expect(container.querySelector('img')).toBeNull();
  expect(container.textContent).not.toContain('never-show-labels');
});

it('describes empty and partial reports without claiming the host has no projects', () => {
  render(<ComposeProjects containers={[]} truncated onSelect={vi.fn()} />);
  expect(screen.getByText('No Compose projects in the reported inventory.')).toBeTruthy();
  expect(screen.getByText(/Projects and counts may be incomplete/)).toBeTruthy();
});
