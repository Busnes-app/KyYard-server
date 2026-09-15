import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Backup } from './Backup';

function mockStatus(body: Record<string, unknown>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (!url.endsWith('/api/backup/status')) throw new Error(`unexpected fetch ${url}`);
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }),
  );
}

// PAIRED is a healthy SQLite instance: the driver decides whether the screen warns that no
// capsule can carry the database.
const PAIRED = {
  key_pinned: true,
  paired: true,
  interval_sec: 86400,
  recovery_url: 'https://recovery.example',
  recovery_key_id: 'k1',
  threshold: 2,
  total_shares: 3,
  database_driver: 'sqlite',
  members: ['data/ky_server.db'],
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Backup', () => {
  it('warns when a key is pinned but there is no destination', async () => {
    mockStatus({ key_pinned: true, paired: false, interval_sec: 0, recovery_key_id: 'k1', threshold: 2, total_shares: 3, database_driver: 'sqlite', members: [] });
    render(<Backup />);
    expect(await screen.findByText(/nowhere to go/i)).toBeTruthy();
    expect(screen.getByText(/automatic backups are off/i)).toBeTruthy();
    expect(screen.getByText(/KY_BACKUP_DIR not set/)).toBeTruthy();
  });

  it('never renders the token', async () => {
    mockStatus({
      key_pinned: true,
      paired: true,
      interval_sec: 86400,
      recovery_url: 'https://recovery.example',
      recovery_key_id: 'k1',
      threshold: 2,
      total_shares: 3,
      database_driver: 'sqlite',
      members: ['data/ky_server.db'],
    });
    const { container } = render(<Backup />);
    expect(await screen.findByText('https://recovery.example')).toBeTruthy();
    expect(container.textContent).not.toMatch(/token/i);
    expect(screen.getByText('data/ky_server.db')).toBeTruthy();
    expect(screen.getByRole('button', { name: /unpair/i })).toBeTruthy();
  });

  it('says so on Postgres, where no capsule carries the database', async () => {
    mockStatus({ ...PAIRED, database_driver: 'postgres' });
    render(<Backup />);
    expect(await screen.findByText(/Backups do not include the PostgreSQL database/)).toBeTruthy();
  });

  it('does not warn about the database on SQLite', async () => {
    mockStatus(PAIRED);
    render(<Backup />);
    await screen.findByText('https://recovery.example');
    expect(screen.queryByText(/Backups do not include the PostgreSQL database/)).toBeNull();
  });
});
