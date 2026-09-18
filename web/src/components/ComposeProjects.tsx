import type { Container } from '../tenant';
import { displayName } from './Endpoints';

// Discovery is an observation from one endpoint snapshot, never an ownership claim.
export function ComposeProjects({ containers, truncated, onSelect }: {
  containers: Container[];
  truncated: boolean;
  onSelect: (project: string) => void;
}) {
  const projects = new Map<string, Container[]>();
  for (const container of containers) {
    if (!container.compose_project) continue;
    const members = projects.get(container.compose_project) ?? [];
    members.push(container);
    projects.set(container.compose_project, members);
  }
  return (
    <section className="panel" aria-label="Compose projects">
      <div className="panel-header"><h2 style={{ fontSize: 16 }}>Compose projects ({projects.size})</h2></div>
      <p>Projects found in this host’s inventory. Unmanaged projects have not been adopted by KyYard.</p>
      {truncated && <p style={{ color: 'var(--ink)' }}>Container inventory is truncated. Projects and counts may be incomplete.</p>}
      {projects.size === 0 && <p>No Compose projects in the reported inventory.</p>}
      {[...projects].sort(([a], [b]) => a.localeCompare(b)).map(([name, members]) => (
        <details key={name} style={{ marginBlock: 12 }}>
          <summary style={{ cursor: 'pointer', overflowWrap: 'anywhere' }}>
            <strong title={name}><bdi>{name}</bdi></strong> · {members.filter((c) => c.state === 'running').length}/{members.length} containers running · <span className="badge">Unmanaged</span>
          </summary>
          <ul className="ky-list">
            {members.map((c) => <li key={c.id} style={{ overflowWrap: 'anywhere' }}>
              <strong>{displayName(c.name)}</strong> · {displayName(c.image)} · {displayName(c.state)}
            </li>)}
          </ul>
          <button className="btn-secondary" style={{ whiteSpace: 'normal', overflowWrap: 'anywhere' }} onClick={() => onSelect(name)}>Show containers for <bdi>{name}</bdi></button>
        </details>
      ))}
    </section>
  );
}
