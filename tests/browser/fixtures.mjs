import { test as base, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Public disposable test credentials, never used for an installed service.
export const initialPassword = 'Browser-Bootstrap-42!';
export const replacementPassword = 'Browser-Replacement-73!';

async function unusedPort() {
  const listener = createServer();
  await new Promise((resolve, reject) => { listener.once('error', reject); listener.listen(0, '127.0.0.1', resolve); });
  const address = listener.address();
  await new Promise((resolve, reject) => listener.close(error => error ? reject(error) : resolve()));
  if (!address || typeof address === 'string') throw new Error('No fixture port');
  return address.port;
}

export const test = base.extend({
  server: async ({}, use) => {
    const binary = process.env.KY_BROWSER_TEST_BINARY;
    if (!binary) throw new Error('Use npm test to build the current fixture server');
    const dir = await mkdtemp(join(tmpdir(), 'kyyard-browser-'));
    const port = await unusedPort();
    const url = `http://127.0.0.1:${port}`;
    // An allowlist prevents this process inheriting live databases, Docker,
    // provider credentials, suite sockets or backup destinations from the host.
    const child = spawn(binary, [], { env: {
      PATH: process.env.PATH,
      KY_HOST: '127.0.0.1', KY_PORT: String(port), KY_APP_URL: url,
      KY_DATA_DIR: join(dir, 'data'), KY_BACKUP_DIR: join(dir, 'backups'),
      KY_DB_DRIVER: 'sqlite', KY_DOCKER_SOCKET: '',
      KY_ADMIN_PASSWORD: initialPassword, KY_CAPTCHA_PROVIDER: 'none'
    }, stdio: ['ignore', 'ignore', 'pipe'] });
    const exited = new Promise(resolve => { child.once('exit', resolve); child.once('error', resolve); });
    let diagnostic = '';
    child.stderr.on('data', chunk => { diagnostic = (diagnostic + chunk).slice(-8192); });
    try {
      await expect.poll(async () => {
        if (child.exitCode !== null) throw new Error('Fixture exited: ' + diagnostic);
        try { return (await fetch(url + '/healthz', { signal: AbortSignal.timeout(1000) })).status; }
        catch { return 0; }
      }, { timeout: 15_000 }).toBe(200);
      await use({ url });
    } finally {
      child.kill('SIGTERM');
      const deadline = setTimeout(() => child.kill('SIGKILL'), 8000);
      await exited;
      clearTimeout(deadline);
      await rm(dir, { recursive: true, force: true });
    }
  },
  baseURL: async ({ server }, use) => use(server.url)
});
export { expect };

export async function signIn(page, password) {
  await page.getByLabel('Username', { exact: true }).fill('admin');
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign In', exact: true }).click();
}

export async function replacePassword(page) {
  await page.getByLabel('Current password', { exact: true }).fill(initialPassword);
  await page.getByLabel('New password', { exact: true }).fill(replacementPassword);
  await page.getByLabel('Confirm new password', { exact: true }).fill(replacementPassword);
  await page.getByRole('button', { name: 'Change password', exact: true }).click();
  await expect(page.getByText('Password changed. Sign in with your new password.')).toBeVisible();
}

export async function enter(page, path = '/') {
  await page.goto(path);
  await signIn(page, initialPassword);
  await expect(page.getByRole('heading', { name: 'Change your password', exact: true })).toBeVisible();
  await replacePassword(page);
  await signIn(page, replacementPassword);
  await expect(page.getByRole('link', { name: 'Home', exact: true })).toBeVisible();
}
