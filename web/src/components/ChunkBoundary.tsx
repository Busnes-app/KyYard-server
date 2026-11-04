import { Component, type ReactNode } from 'react';

// ChunkBoundary keeps a lazy part that failed to load (a stale tab after an upgrade, a dropped
// connection) from unmounting the whole app: it shows a notice with Reload instead.
export class ChunkBoundary extends Component<{ what: string; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() {
    if (!this.state.failed) return this.props.children;
    return <div role="alert"><p>The {this.props.what} could not load. Reload the page to get the current version.</p><button type="button" className="btn-secondary" onClick={() => window.location.reload()}>Reload</button></div>;
  }
}
