import type { Port } from '../tenant';

export function ContainerPorts({ ports }: { ports: Port[] }) {
  const mappings = new Map<string, Set<string>>();
  for (const port of ports) {
    const mapping = port.host ? `${port.host} → ${port.container}/${port.protocol}` : `${port.container}/${port.protocol}`;
    const addresses = mappings.get(mapping) ?? new Set<string>();
    addresses.add(port.host ? (port.host_ip === '0.0.0.0' ? 'All IPv4' : port.host_ip === '::' ? 'All IPv6' : port.host_ip || 'Host address not reported') : 'Not published');
    mappings.set(mapping, addresses);
  }
  return ports.length ? <ul className="ky-container-ports" aria-label="Port mappings">{Array.from(mappings, ([mapping, addresses]) => <li key={mapping}><span>{mapping}</span><small>{Array.from(addresses).join(' · ')}</small></li>)}</ul> : <span className="ky-muted">None reported</span>;
}
