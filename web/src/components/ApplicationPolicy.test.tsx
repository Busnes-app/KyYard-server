import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ApplicationPolicy, RUN_OUTCOMES } from './ApplicationPolicy';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = 'ky_csrf=; Max-Age=0'; });

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
const policy = (over: Record<string, unknown> = {}) => ({ id: 'p', application_id: 'app', created_by: 'usr_a', mode: 'apply', timezone: 'Europe/Paris', weekdays: [1, 3], start_minute: 120, end_minute: 240, status: 'active', paused_reason: '', consecutive_failures: 0, created_at: '2026-09-24T10:00:00Z', updated_at: '2026-09-24T10:00:00Z', next_occurrence: '2026-09-28T00:00:00Z', runs: [], ...over });
const run = (over: Record<string, unknown>) => ({ id: 'r', policy_id: 'p', occurrence: '2026-09-24T00:00:00Z', started_at: '2026-09-24T00:00:05Z', finished_at: '2026-09-24T00:00:09Z', outcome: 'applied', deployment_id: '', detail: '', correlation_id: 'c', ...over });
const windowText = 'The window must end after it starts, last at least 15 minutes and end by midnight; windows cannot cross midnight.';
function stub(get: () => Response, write?: (url: string, init: RequestInit) => Response) {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => init?.method ? (write ? write(url, init) : json({})) : get());
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const open = () => fireEvent.click(screen.getByRole('button', { name: 'Update policy' }));
const save = () => fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

it('requests nothing until opened, then offers an administrator a new policy', async () => {
  const fetcher = stub(() => json({ error: 'none' }, 404));
  render(<ApplicationPolicy base="/app" admin />);
  expect(fetcher).not.toHaveBeenCalled();
  open();
  await screen.findByText('No update policy.');
  expect(fetcher.mock.calls[0][0]).toBe('/app/update-policy');
  expect(screen.getByRole('button', { name: 'Save policy' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Delete policy' })).toBeNull();
});

it('shows other members the policy without the editor', async () => {
  stub(() => json(policy()));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Mon, Wed 02:00–04:00 \(Europe\/Paris\)/);
  expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Delete policy' })).toBeNull();
});

it('refuses an empty day set and a short or reversed window before sending', async () => {
  const fetcher = stub(() => json({}, 404));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  for (const day of ['Mon', 'Tue', 'Wed', 'Thu', 'Fri']) fireEvent.click(screen.getByLabelText(day));
  save();
  expect(await screen.findByText('Choose at least one day.')).toBeTruthy();
  fireEvent.click(screen.getByLabelText('Sat'));
  fireEvent.change(screen.getByLabelText('Window start'), { target: { value: '10:00' } });
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '10:10' } });
  save();
  expect(await screen.findByText(windowText)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '09:00' } });
  save();
  expect(await screen.findByText(windowText)).toBeTruthy();
  expect(fetcher.mock.calls.every((c) => !c[1]?.method)).toBe(true);
});

it('saves with the CSRF header in the shape the server expects; 00:00 ends at midnight', async () => {
  document.cookie = 'ky_csrf=csrf';
  let saved: unknown = null;
  vi.spyOn(Intl, 'supportedValuesOf').mockReturnValue(['Europe/Paris', 'Asia/Tokyo']);
  const fetcher = stub(() => saved ? json(policy()) : json({}, 404), (_url, init) => { saved = JSON.parse(String(init.body)); return json(policy(), 201); });
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  fireEvent.change(screen.getByLabelText('Mode'), { target: { value: 'apply' } });
  fireEvent.change(screen.getByLabelText('Window start'), { target: { value: '23:00' } });
  fireEvent.change(screen.getByLabelText('Window end'), { target: { value: '00:00' } });
  fireEvent.change(screen.getByLabelText('Time zone'), { target: { value: 'Asia/Tokyo' } });
  save();
  await screen.findByText(/Plan and apply ·/);
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(put?.[0]).toBe('/app/update-policy');
  expect(new Headers(put?.[1]?.headers).get('X-CSRF-Token')).toBe('csrf');
  expect(saved).toEqual({ mode: 'apply', timezone: 'Asia/Tokyo', weekdays: [1, 2, 3, 4, 5], start_minute: 1380, end_minute: 1440 });
});

it('maps each refusal code to fixed text and never shows server text', async () => {
  const texts: Record<string, string> = {
    invalid_timezone: 'The server does not recognise this time zone. Choose another.',
    invalid_window: windowText,
    invalid_weekdays: 'Choose at least one day.',
    invalid_mode: 'Choose a mode.',
  };
  let code = '';
  stub(() => json({}, 404), () => json({ error: 'secret-canary', code }, 400));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText('No update policy.');
  for (const [c, text] of Object.entries(texts)) {
    code = c;
    save();
    expect(await screen.findByText(text)).toBeTruthy();
  }
  code = 'made_up';
  save();
  expect(await screen.findByText('The policy was refused as invalid.')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
});

// Review Focus 2: the browser's list lacks aliases and UTC; a saved zone must survive the editor.
it('keeps a saved zone this browser does not list, and defaults a new policy to the browser zone', async () => {
  vi.spyOn(Intl, 'supportedValuesOf').mockReturnValue(['Europe/Paris']);
  stub(() => json(policy({ timezone: 'US/Eastern' })));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  const saved = await screen.findByLabelText('Time zone') as HTMLSelectElement;
  expect(saved.value).toBe('US/Eastern');
  expect(Array.from(saved.options).map((o) => o.value)).toContain('Europe/Paris');
  cleanup();
  const real = new Intl.DateTimeFormat().resolvedOptions();
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({ ...real, timeZone: 'Asia/Tokyo' });
  stub(() => json({}, 404));
  render(<ApplicationPolicy base="/app" admin />);
  open();
  expect(((await screen.findByLabelText('Time zone')) as HTMLSelectElement).value).toBe('Asia/Tokyo');
});

it('renders runs through fixed tables only', async () => {
  const deployment = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  stub(() => json(policy({ runs: [
    run({ id: 'a', outcome: 'applied', deployment_id: deployment }),
    run({ id: 'b', outcome: 'blocked', detail: 'configuration_unsupported,made_up_blocker' }),
    run({ id: 'c', outcome: 'failed', detail: 'unauthorized' }),
    run({ id: 'd', outcome: 'skipped_missed', detail: 'the server was not running during this window' }),
    run({ id: 'e', outcome: 'failed', detail: 'secret-canary' }),
    run({ id: 'f', outcome: 'exploded' }),
    run({ id: 'g', outcome: 'paused', detail: 'three consecutive windows failed' }),
  ] })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(RUN_OUTCOMES.applied);
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(deployment);
  expect(screen.getByText(/configuration the definition cannot express/)).toBeTruthy();
  expect(screen.getByText('The registry refused the credentials.')).toBeTruthy();
  expect(screen.getByText('The server was not running during this window.')).toBeTruthy();
  expect(screen.getByText('Unrecognised outcome')).toBeTruthy();
  expect(screen.getByText('Three windows in a row failed. Fix the cause, then resume.')).toBeTruthy();
  expect(document.body.textContent).not.toContain('secret-canary');
  expect(document.body.textContent).not.toContain('made_up_blocker');
});

it('shows why a policy paused and lets only an administrator resume it', async () => {
  document.cookie = 'ky_csrf=csrf';
  let paused = true;
  const get = () => json(policy(paused ? { status: 'paused', paused_reason: 'three consecutive windows failed', next_occurrence: null, consecutive_failures: 3 } : {}));
  const fetcher = stub(get, () => { paused = false; return json(policy()); });
  render(<ApplicationPolicy base="/app" admin />);
  open();
  await screen.findByText(/Three windows in a row failed/);
  fireEvent.click(screen.getByRole('button', { name: 'Resume policy' }));
  await screen.findByText(/Next window/);
  expect(fetcher.mock.calls.find((c) => c[1]?.method === 'POST')?.[0]).toBe('/app/update-policy/resume');
  cleanup();
  paused = true;
  stub(get);
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Three windows in a row failed/);
  expect(screen.queryByRole('button', { name: 'Resume policy' })).toBeNull();
});

it('shows the next window in the policy zone and the browser zone', async () => {
  stub(() => json(policy({ timezone: 'Asia/Tokyo', next_occurrence: '2026-09-28T00:00:00Z' })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  const status = await screen.findByText(/Next window/);
  expect(status.textContent).toContain('09:00'); // 00:00 UTC is 09:00 in Tokyo
  expect(status.textContent).toContain('your time');
});

// Fix round 1: Intl.supportedValuesOf is absent on some engines; the editor must not throw.
it('falls back to a typed zone when Intl.supportedValuesOf is unavailable', async () => {
  document.cookie = 'ky_csrf=csrf';
  const partialIntl = Object.create(Intl) as typeof Intl;
  Object.defineProperty(partialIntl, 'supportedValuesOf', { value: undefined });
  vi.stubGlobal('Intl', partialIntl);
  const real = new Intl.DateTimeFormat().resolvedOptions();
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({ ...real, timeZone: 'Asia/Tokyo' });
  let saved: unknown = null;
  const fetcher = stub(() => saved ? json(policy()) : json(policy({ timezone: 'Europe/Paris' })), (_url, init) => { saved = JSON.parse(String(init.body)); return json(policy(), 201); });
  render(<ApplicationPolicy base="/app" admin />);
  expect(() => open()).not.toThrow();
  await screen.findByText(/Mon, Wed/);
  const select = await screen.findByLabelText('Time zone') as HTMLSelectElement;
  expect(Array.from(select.options).map((o) => o.value).sort()).toEqual(['Asia/Tokyo', 'Europe/Paris']);
  fireEvent.change(screen.getByLabelText('Other timezone'), { target: { value: 'Pacific/Auckland' } });
  save();
  await screen.findByText(/Plan and apply ·/);
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect((saved as { timezone: string }).timezone).toBe('Pacific/Auckland');
  expect(put).toBeTruthy();
});

const endpoint = (capabilities: string[]) => ({ id: 'host', environment_id: 'env', name: 'Docker', runtime: 'docker', state: 'active', facts: {}, fingerprint: 'f', capabilities, alerts: [], created_at: '2026-09-24T10:00:00Z' });
function stubWithEndpoint(p: unknown, capabilities: string[]) {
  const fetcher = vi.fn(async (url: string) => String(url).includes('/endpoints/') ? json(endpoint(capabilities)) : json(p));
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
const isEndpoint = (c: unknown[]) => c[0] === '/api/organizations/a/endpoints/host';

it('warns that a host without health reporting cannot validate automated updates', async () => {
  const fetcher = stubWithEndpoint(policy(), ['container.inspect', 'container.inspect.verdict']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await screen.findByText(/cannot report container health/);
  expect(fetcher.mock.calls.some(isEndpoint)).toBe(true);
});

it('does not warn for a host that reports health, and asks nothing for a plan-only policy', async () => {
  const fetcher = stubWithEndpoint(policy(), ['container.inspect', 'container.inspect.health']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await vi.waitFor(() => expect(fetcher.mock.calls.some(isEndpoint)).toBe(true));
  await screen.findByText(/Plan and apply ·/);
  expect(screen.queryByText(/cannot report container health/)).toBeNull();
  cleanup();
  const planOnly = stubWithEndpoint(policy({ mode: 'plan_only' }), ['container.inspect']);
  render(<ApplicationPolicy base="/app" admin={false} org="a" endpointID="host" />);
  open();
  await screen.findByText(/Plan only; apply by hand ·/);
  expect(planOnly.mock.calls.some(isEndpoint)).toBe(false);
});

it('shows each run validation and its rollback, and explains a validation pause', async () => {
  const rolledBack = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  const v = (over: Record<string, unknown>) => ({ deployment_id: 'd', policy_run_id: 'r', automated: true, is_rollback: false, phase: 'done', started_at: '2026-09-24T10:00:00Z', observe_until: '2026-09-24T10:02:30Z', verdict: 'unhealthy', detail: 'web', rollback: null, correlation_id: 'c', finished_at: '2026-09-24T10:00:31Z', ...over });
  stub(() => json(policy({ status: 'paused', next_occurrence: null, paused_reason: 'update failed and could not be rolled back: prior_images_missing', runs: [
    run({ id: 'a', validation: v({ rollback: { deployment_id: rolledBack, revision: 3, outcome: 'applied', detail: '' } }) }),
    run({ id: 'b', validation: v({ verdict: 'exited', detail: 'db', rollback: { deployment_id: '', revision: 0, outcome: 'ineligible', detail: 'prior_images_missing' } }) }),
    run({ id: 'c', outcome: 'no_update' }),
  ] })));
  render(<ApplicationPolicy base="/app" admin={false} />);
  open();
  await screen.findByText(/Rolled back to revision 3\./);
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(rolledBack);
  expect(screen.getAllByText(/The earlier images are no longer on the host\./).length).toBe(2);
  expect(screen.getByText(/failed validation and was not rolled back/)).toBeTruthy();
});
