import { test, expect, enter, initialPassword, replacementPassword, signIn, replacePassword } from './fixtures.mjs';

test('bootstrap requires a password replacement and revokes the old login', async ({ page }) => {
  await page.goto('/');
  await signIn(page, initialPassword);
  await expect(page.getByRole('heading', { name: 'Change your password', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Home', exact: true })).toHaveCount(0);
  const oldCookies = (await page.context().cookies()).map(cookie => `${cookie.name}=${cookie.value}`).join('; ');
  await page.getByLabel('Current password', { exact: true }).fill(initialPassword);
  await page.getByLabel('New password', { exact: true }).fill(replacementPassword);
  await page.getByLabel('Confirm new password', { exact: true }).fill('Different-Password-55!');
  await page.getByRole('button', { name: 'Change password', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText('The new passwords do not match.');
  await replacePassword(page);
  const oldSession = await page.request.get('/api/auth/me', { headers: { Cookie: oldCookies } });
  expect((await oldSession.json()).authenticated).not.toBe(true);
  const refused = page.waitForResponse(response => response.url().endsWith('/api/auth/login'));
  await signIn(page, initialPassword);
  expect((await refused).status()).toBe(401);
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible();
  await signIn(page, replacementPassword);
  await expect(page.getByRole('link', { name: 'Home', exact: true })).toBeVisible();
});

test('authenticated deep links and logout use real session cookies', async ({ page, context }) => {
  await enter(page, '/settings?section=storage');
  await expect(page.getByRole('heading', { name: 'Storage', exact: true })).toBeVisible();
  const cookies = await context.cookies();
  expect(cookies.find(cookie => cookie.name === 'ky_session')).toMatchObject({ httpOnly: true, sameSite: 'Lax' });
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Storage', exact: true })).toBeVisible();
  await page.getByRole('button', { name: /sign out|log out/i }).click();
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible();
  const response = await page.request.get('/api/auth/me');
  const body = await response.json();
  expect(body.authenticated).not.toBe(true);
});

test('real environment form persists a CSRF-protected tenant write', async ({ page }) => {
  await enter(page);
  const response = await page.request.get('/api/organizations');
  expect(response.ok()).toBe(true);
  const organizations = await response.json();
  expect(Array.isArray(organizations)).toBe(true);
  expect(typeof organizations[0]?.id).toBe('string');
  const path = `/organizations/${organizations[0].id}`;
  const refused = await page.request.post(`/api/organizations/${organizations[0].id}/environments`, {
    data: { name: 'Missing CSRF must not persist' }
  });
  expect(refused.status()).toBe(403);
  await page.goto(path);
  await page.getByLabel('New environment', { exact: true }).fill('Browser environment');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page.getByRole('link', { name: 'Browser environment', exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('link', { name: 'Browser environment', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Missing CSRF must not persist', exact: true })).toHaveCount(0);
});

test('Appearance follows system, persists a choice and synchronizes tabs', async ({ page, context }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await enter(page, '/appearance');
  const busnes = page.getByRole('group', { name: 'Busnes', exact: true });
  await expect(busnes.getByRole('radio', { name: 'Match system' })).toBeChecked();
  await expect(page.locator('html')).toHaveCSS('color-scheme', 'dark');
  await busnes.getByRole('radio', { name: 'Light' }).check();
  await page.reload();
  await expect(page.getByRole('group', { name: 'Busnes', exact: true }).getByRole('radio', { name: 'Light' })).toBeChecked();
  await expect(page.locator('html')).toHaveCSS('color-scheme', 'light');
  const other = await context.newPage();
  await other.goto('/appearance');
  const otherBusnes = other.getByRole('group', { name: 'Busnes', exact: true });
  await expect(otherBusnes.getByRole('radio', { name: 'Light' })).toBeChecked();
  await otherBusnes.getByRole('radio', { name: 'Dark' }).check();
  await expect(page.getByRole('group', { name: 'Busnes', exact: true }).getByRole('radio', { name: 'Dark' })).toBeChecked();
  await expect(page.locator('html')).toHaveCSS('color-scheme', 'dark');
});

test('duplicate environment refusal leaves the form usable', async ({ page }) => {
  await enter(page);
  const organizations = await (await page.request.get('/api/organizations')).json();
  await page.goto(`/organizations/${organizations[0].id}`);
  const name = page.getByLabel('New environment', { exact: true });
  await name.fill('Repeated environment');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page.getByRole('link', { name: 'Repeated environment', exact: true })).toBeVisible();
  await name.fill('Repeated environment');
  const response = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/environments'));
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  expect([400, 409]).toContain((await response).status());
  await expect(page.getByRole('alert')).toBeVisible();
  await name.fill('Recovered environment');
  await page.getByRole('button', { name: 'Create', exact: true }).click();
  await expect(page.getByRole('link', { name: 'Recovered environment', exact: true })).toBeVisible();
});

test('unassigned tenant deep links refuse access and offer no write form', async ({ page, context }) => {
  await enter(page);
  const csrf = (await context.cookies()).find(cookie => cookie.name === 'ky_csrf');
  expect(csrf).toBeDefined();
  const headers = { 'X-CSRF-Token': csrf.value, Origin: new URL(page.url()).origin };
  const user = await page.request.post('/api/admin/users', {
    headers, data: { username: 'other-owner', display_name: 'Other owner', role: 'user' }
  });
  expect(user.status()).toBe(201);
  const organization = await page.request.post('/api/admin/organizations', {
    headers, data: { name: 'Other tenant', admin_user_id: (await user.json()).id }
  });
  expect(organization.status()).toBe(201);
  const { id } = await organization.json();
  const own = await (await page.request.get('/api/organizations')).json();
  expect(own.some(item => item.id === id)).toBe(false);
  expect((await page.request.get(`/api/organizations/${id}/environments`)).status()).toBe(403);
  await page.goto(`/organizations/${id}`);
  await expect(page.getByText('You do not have access to this section. Ask an administrator for access.')).toBeVisible();
  await expect(page.getByLabel('New environment', { exact: true })).toHaveCount(0);
});

test('logout revokes another open tab without restoring its session', async ({ page, context }) => {
  await enter(page, '/settings');
  const oldCookies = (await context.cookies()).map(cookie => `${cookie.name}=${cookie.value}`).join('; ');
  const other = await context.newPage();
  await other.goto('/settings?section=storage');
  await expect(other.getByRole('heading', { name: 'Storage', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible();
  await other.reload();
  await expect(other.getByLabel('Username', { exact: true })).toBeVisible();
  await expect(other.getByRole('heading', { name: 'Storage', exact: true })).toHaveCount(0);
  const oldSession = await page.request.get('/api/auth/me', { headers: { Cookie: oldCookies } });
  expect((await oldSession.json()).authenticated).not.toBe(true);
});

test('keyboard navigation and Settings fit the viewport', async ({ page }) => {
  await enter(page, '/settings');
  const endpoints = page.getByRole('link', { name: 'Endpoints', exact: true });
  await endpoints.focus();
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/endpoints$/);
  await page.getByRole('link', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();
  await page.getByRole('link', { name: 'Appearance', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Appearance', exact: true })).toBeVisible();
  const width = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(width).toBeLessThanOrEqual(page.viewportSize().width + 1);
});
