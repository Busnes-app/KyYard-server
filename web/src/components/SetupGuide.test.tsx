import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { SetupGuide } from './SetupGuide';

// The guide is the only place the product itself tells an operator what to keep private. It
// has to name the port they are actually reachable on, or they firewall something else.
test('names the port of the origin it was given', () => {
  render(<SetupGuide appURL="http://localhost:9273" />);
  expect(screen.getByText(/Keep port 9273 private/)).toBeTruthy();
  expect(screen.getByText(/SSH tunnel to localhost:9273/)).toBeTruthy();
});

test('an origin with no port is port 80, and HTTPS needs no guide at all', () => {
  const { container } = render(<SetupGuide appURL="http://kyyard.internal" />);
  expect(container.textContent).toContain('Keep port 80 private');
  const secure = render(<SetupGuide appURL="https://kyyard.example.com" />);
  expect(secure.container.textContent).toBe('');
});
