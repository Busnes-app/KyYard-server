import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Registries } from './Registries';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const ghcr = { id: 'reg-1', organization_id: 'a', host: 'ghcr.io', name: 'GitHub', username: 'bot', has_credential: true, allow_private: false };
const lan = { id: 'reg-2', organization_id: 'a', host: 'registry.lan:5000', name: 'LAN', username: '', has_credential: false, allow_private: true };

type Write = { url: string; method: string; body?: unknown; csrf: string | null };

// A fake server: GETs read the current state, writes are recorded and applied by `apply`.
function serve(rows: unknown[], anonymous = false, writeStatus = 0, privateEnabled = true) {
  const writes: Write[] = [];
  const state = { rows: [...rows], anonymous };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? 'GET';
    if (method === 'GET') {
      if (url === '/api/organizations/a/registries') return json(state.rows);
      if (url === '/api/organizations/a/registry-policy') return json({ anonymous_pull_enabled: state.anonymous, private_registries_enabled: privateEnabled });
      return json({ error: 'not found' }, 404);
    }
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    writes.push({ url, method, body, csrf: new Headers(init?.headers).get('X-CSRF-Token') });
    if (writeStatus) return json({ error: '<b>secret server detail</b>' }, writeStatus);
    if (url.endsWith('/registry-policy')) { state.anonymous = body.anonymous_pull_enabled; return new Response(null, { status: 204 }); }
    if (method === 'DELETE') { state.rows = state.rows.filter((r) => !url.endsWith(`/${(r as { id: string }).id}`)); return new Response(null, { status: 204 }); }
    const row = { id: 'reg-new', organization_id: 'a', host: body.host, name: body.name, username: body.username, has_credential: body.credential !== undefined && body.credential !== '', allow_private: body.allow_private };
    state.rows = [...state.rows.filter((r) => (r as { host: string }).host !== body.host), row];
    return json(row);
  }));
  document.cookie = 'ky_csrf=csrf-test';
  return writes;
}

it('lists registries with their credential and private-address flags', async () => {
  serve([ghcr, lan]);
  render(<Registries org="a" />);
  expect(await screen.findByText('ghcr.io')).toBeTruthy();
  expect(screen.getByText('registry.lan:5000')).toBeTruthy();
  expect(screen.getByText('credential set')).toBeTruthy();
  expect(screen.getByText('no credential')).toBeTruthy();
  expect(screen.getByText('private addresses allowed')).toBeTruthy();
});

it('shows the empty state', async () => {
  serve([]);
  render(<Registries org="a" />);
  expect(await screen.findByText(/No registries/)).toBeTruthy();
});

it('adds a registry with a credential and clears the field', async () => {
  const writes = serve([]);
  render(<Registries org="a" />);
  await screen.findByText(/No registries/);
  const credential = screen.getByLabelText('Credential') as HTMLInputElement;
  expect(credential.type).toBe('password');
  expect(credential.autocomplete).toBe('new-password');
  fireEvent.change(screen.getByLabelText('Host'), { target: { value: 'ghcr.io' } });
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'GitHub' } });
  fireEvent.change(screen.getByLabelText('Username'), { target: { value: 'bot' } });
  fireEvent.change(credential, { target: { value: 's3cret' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  expect(await screen.findByText('credential set')).toBeTruthy();
  expect(writes).toEqual([{ url: '/api/organizations/a/registries', method: 'PUT', csrf: 'csrf-test', body: { host: 'ghcr.io', name: 'GitHub', username: 'bot', allow_private: false, credential: 's3cret' } }]);
  expect((screen.getByLabelText('Credential') as HTMLInputElement).value).toBe('');
});

it('updates without touching the credential and sends no credential key', async () => {
  const writes = serve([ghcr]);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Edit ghcr.io' }));
  expect((screen.getByLabelText('Host') as HTMLInputElement).value).toBe('ghcr.io');
  expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('bot');
  expect((screen.getByLabelText('Credential') as HTMLInputElement).value).toBe('');
  fireEvent.click(await screen.findByLabelText('Allow private addresses'));
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0].body).toEqual({ host: 'ghcr.io', name: 'GitHub', username: 'bot', allow_private: true });
  expect(Object.hasOwn(writes[0].body as object, 'credential')).toBe(false);
});

it('clear sends an empty credential', async () => {
  const writes = serve([ghcr]);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Edit ghcr.io' }));
  fireEvent.click(screen.getByLabelText('Clear credential'));
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  expect(await screen.findByText('no credential')).toBeTruthy();
  expect(writes[0].body).toEqual({ host: 'ghcr.io', name: 'GitHub', username: 'bot', allow_private: false, credential: '' });
});

it('deletes after confirmation and refreshes', async () => {
  const writes = serve([ghcr]);
  const confirm = vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Delete ghcr.io' }));
  expect(writes).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: 'Delete ghcr.io' }));
  expect(await screen.findByText(/No registries/)).toBeTruthy();
  expect(confirm).toHaveBeenCalledTimes(2);
  expect(writes).toEqual([{ url: '/api/organizations/a/registries/reg-1', method: 'DELETE', csrf: 'csrf-test', body: undefined }]);
});

it('the anonymous-pull switch confirms turning on, sends PUT and re-renders', async () => {
  const writes = serve([]);
  const confirm = vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true);
  render(<Registries org="a" />);
  const toggle = await screen.findByRole('switch', { name: 'Allow anonymous pulls' }) as HTMLInputElement;
  expect(toggle.checked).toBe(false);
  expect(screen.getByText('Off: images from hosts without a registry entry cannot be pulled. On: anonymous pulls are allowed; audited.')).toBeTruthy();
  fireEvent.click(toggle);
  expect(confirm).toHaveBeenLastCalledWith('Allow anonymous pulls from hosts without a registry entry? This weakens the default and is audited.');
  expect(writes).toHaveLength(0);
  expect(toggle.checked).toBe(false);
  fireEvent.click(toggle);
  await waitFor(() => expect((screen.getByRole('switch', { name: 'Allow anonymous pulls' }) as HTMLInputElement).checked).toBe(true));
  expect(writes).toEqual([{ url: '/api/organizations/a/registry-policy', method: 'PUT', csrf: 'csrf-test', body: { anonymous_pull_enabled: true } }]);
  // Turning it off restores the default and needs no confirmation.
  fireEvent.click(screen.getByRole('switch', { name: 'Allow anonymous pulls' }));
  await waitFor(() => expect((screen.getByRole('switch', { name: 'Allow anonymous pulls' }) as HTMLInputElement).checked).toBe(false));
  expect(confirm).toHaveBeenCalledTimes(2);
  expect(writes[1].body).toEqual({ anonymous_pull_enabled: false });
});

it('403 shows the fixed refusal and never the response body', async () => {
  serve([ghcr], false, 403);
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('switch', { name: 'Allow anonymous pulls' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Only an organization administrator can manage registries.');
  expect((screen.getByRole('switch', { name: 'Allow anonymous pulls' }) as HTMLInputElement).checked).toBe(false);
  expect(document.body.textContent).not.toContain('secret server detail');
});

it('other failures show generic fixed text', async () => {
  serve([ghcr], false, 500);
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Delete ghcr.io' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Request failed (500).');
  expect(document.body.textContent).not.toContain('secret server detail');
});

it('a failed save still empties the credential field', async () => {
  const writes = serve([], false, 500);
  render(<Registries org="a" />);
  await screen.findByText(/No registries/);
  fireEvent.change(screen.getByLabelText('Host'), { target: { value: 'ghcr.io' } });
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'GitHub' } });
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: 's3cret' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Request failed (500).');
  expect((screen.getByLabelText('Credential') as HTMLInputElement).value).toBe('');
  expect(writes).toHaveLength(1);
  expect((writes[0].body as { credential: string }).credential).toBe('s3cret');
});

it('400 names the fields and the credential limit', async () => {
  serve([], false, 400);
  render(<Registries org="a" />);
  await screen.findByText(/No registries/);
  fireEvent.change(screen.getByLabelText('Host'), { target: { value: 'ghcr.io' } });
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'GitHub' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Check the host, name and credential (at most 4096 bytes).');
  expect(document.body.textContent).not.toContain('secret server detail');
});

it('edit mode names the entry and Cancel resets the form', async () => {
  serve([ghcr]);
  render(<Registries org="a" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Edit ghcr.io' }));
  expect(screen.getByText('Editing ghcr.io')).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: 'typed' } });
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  expect(screen.queryByText('Editing ghcr.io')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
  for (const field of ['Host', 'Name', 'Username', 'Credential']) expect((screen.getByLabelText(field) as HTMLInputElement).value).toBe('');
});

it('hides the private-address switch when the operator has not allowed it', async () => {
  const writes = serve([lan], false, 0, false);
  render(<Registries org="a" />);
  expect(await screen.findByText('Private-address registries are disabled by the operator (KY_REGISTRY_ALLOW_PRIVATE).')).toBeTruthy();
  expect(screen.queryByLabelText('Allow private addresses')).toBeNull();
  // Editing a row stored with the flag drops it rather than sending a refused value.
  fireEvent.click(screen.getByRole('button', { name: 'Edit registry.lan:5000' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save registry' }));
  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0].body).toEqual({ host: 'registry.lan:5000', name: 'LAN', username: '', allow_private: false });
});

it('shows the private-address switch when the operator allows it', async () => {
  serve([], false, 0, true);
  render(<Registries org="a" />);
  expect(await screen.findByLabelText('Allow private addresses')).toBeTruthy();
  expect(screen.queryByText(/disabled by the operator/)).toBeNull();
});

it('a private_registries_disabled refusal shows the operator text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'x', code: 'private_registries_disabled' }, 403)));
  document.cookie = 'ky_csrf=csrf-test';
  const { tenantWrite } = await import('../tenant');
  expect(await tenantWrite('/api/organizations/a/registries', 'PUT', {}, { forbidden: 'no' })).toBe('Private-address registries are disabled by the operator (KY_REGISTRY_ALLOW_PRIVATE).');
});
