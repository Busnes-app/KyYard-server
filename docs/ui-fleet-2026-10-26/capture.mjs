// KyYard adapter for capture-core.mjs: a fresh SQLite server with no Docker socket or
// enrolled endpoints, an init-admin administrator with a generated password, signed in
// through the real login form.
import { randomBytes } from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { here, run, waitReady } from './capture-core.mjs';

const repo = join(here(import.meta.url), '../..');
const PORT = '5872';
const baseURL = `http://127.0.0.1:${PORT}`;
const secret = () => `Fleet-${randomBytes(18).toString('base64url')}9!`;
let password = secret();
const sh = (cmd, args, opts) => { const r = spawnSync(cmd, args, { stdio: ['pipe', 'ignore', 'inherit'], ...opts }); if (r.status !== 0) throw new Error(`${cmd} ${args[0]} exited ${r.status}`); };

await run({
  product: 'kyyard',
  repo,
  dist: join(repo, 'web/dist/index.html'),
  fixture: 'web built from this tree; fresh SQLite data dir; captcha none; KY_DOCKER_SOCKET empty (no local Docker endpoint), no enrolled endpoints or organizations beyond first-run defaults; init-admin "admin" (generated password on stdin, replaced through /api/auth/change-password when required)',
  async start(dir) {
    sh('npm', ['run', 'build'], { cwd: join(repo, 'web'), stdio: ['ignore', 'ignore', 'inherit'] });
    const binary = join(dir, 'kyyard-server');
    sh('go', ['build', '-o', binary, './cmd/server'], { cwd: repo });
    const env = {
      PATH: process.env.PATH, HOME: dir, KY_APP_URL: baseURL, KY_HOST: '127.0.0.1', KY_PORT: PORT,
      KY_DB_DRIVER: 'sqlite', KY_DATA_DIR: join(dir, 'data'), KY_BACKUP_DIR: join(dir, 'backups'),
      KY_CAPTCHA_PROVIDER: 'none', KY_SUITE_RUNNER_SOCKET: join(dir, 'no-runner.sock'),
      // Empty disables the automatic local Docker endpoint: never read this host's containers.
      KY_DOCKER_SOCKET: '',
    };
    sh(binary, ['init-admin', '-username', 'admin'], { cwd: dir, env, input: `${password}\n` });
    const proc = spawn(binary, [], { cwd: dir, env, stdio: ['ignore', 'ignore', 'inherit'] });
    await waitReady(`${baseURL}/`);
    // First sign-in may require a new password; replace it through the real API once.
    const login = await fetch(`${baseURL}/api/auth/login`, { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: baseURL }, body: JSON.stringify({ username: 'admin', password }) });
    const state = await login.json().catch(() => ({}));
    if (state.must_change_password ?? state.user?.must_change_password) {
      const cookies = login.headers.getSetCookie().map((c) => c.split(';')[0]);
      const csrf = cookies.find((c) => c.startsWith('ky_csrf='))?.split('=')[1];
      const next = secret();
      const changed = await fetch(`${baseURL}/api/auth/change-password`, { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: baseURL, Cookie: cookies.join('; '), 'X-CSRF-Token': csrf }, body: JSON.stringify({ current_password: password, new_password: next }) });
      if (!changed.ok) throw new Error(`change-password ${changed.status}`);
      password = next;
    }
    return { baseURL, binary, pid: proc.pid, stop: async () => { proc.kill('SIGTERM'); await new Promise((r) => proc.once('exit', r)); } };
  },
  async signIn(page) {
    await page.goto('/');
    await page.getByLabel('Username', { exact: true }).fill('admin');
    await page.getByLabel('Password', { exact: true }).fill(password);
    await page.getByRole('button', { name: /sign in/i }).first().click();
    await page.getByRole('navigation', { name: 'Primary' }).waitFor();
  },
  surfaces: {
    async home(page) {
      await page.goto('/');
      await page.getByRole('heading', { level: 1 }).first().waitFor();
      await page.waitForTimeout(800);
    },
    async appearance(page) {
      await page.goto('/settings?section=appearance');
      await page.getByRole('heading', { name: 'Appearance' }).first().waitFor();
    },
  },
});
