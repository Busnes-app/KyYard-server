import { spawnSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
const dir = await mkdtemp(join(tmpdir(), 'kyyard-browser-build-'));
try {
  const binary = join(dir, 'kyyard-server');
  const built = spawnSync('go', ['build', '-o', binary, './cmd/server'], { cwd: root, stdio: 'inherit' });
  if (built.error) throw built.error;
  if (built.status !== 0) throw new Error('Browser fixture server build failed');
  const result = spawnSync(process.execPath,
    [fileURLToPath(new URL('node_modules/@playwright/test/cli.js', import.meta.url)), 'test', ...process.argv.slice(2)],
    { cwd: fileURLToPath(new URL('.', import.meta.url)), stdio: 'inherit', env: { ...process.env, KY_BROWSER_TEST_BINARY: binary } });
  if (result.error) throw result.error;
  process.exitCode = result.status ?? 1;
} finally {
  await rm(dir, { recursive: true, force: true });
}
