import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { ContainerPorts } from './ContainerPorts';
afterEach(cleanup);
it('groups dual-stack mappings without hiding specific bindings or unpublished ports', () => {
  render(<ContainerPorts ports={[
    { host: 2342, container: 2342, protocol: 'tcp', host_ip: '0.0.0.0' },
    { host: 2342, container: 2342, protocol: 'tcp', host_ip: '::' },
    { host: 2342, container: 2342, protocol: 'tcp', host_ip: '127.0.0.1' },
    { host: 2342, container: 2342, protocol: 'udp', host_ip: '::1' },
    { container: 2442, protocol: 'tcp' },
  ]} />);
  expect(screen.getAllByRole('listitem')).toHaveLength(3);
  expect(screen.getAllByText('2342 → 2342/tcp')).toHaveLength(1);
  expect(screen.getByText('All IPv4 · All IPv6 · 127.0.0.1')).toBeTruthy();
  expect(screen.getByText('::1')).toBeTruthy();
  expect(screen.getByText('Not published')).toBeTruthy();
});
