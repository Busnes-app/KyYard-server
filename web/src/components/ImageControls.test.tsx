import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ImageControls } from './ImageControls';
const imageID = `sha256:${'a'.repeat(64)}`;
const base = '/api/organizations/team/endpoints/host';
const props = { base, active: true, scope: 'Team / Production / Host', onActivity: vi.fn() };
const inventory = (containers: unknown[] = [], truncated: string[] = []) => ({ received_at: new Date().toISOString(), snapshot: { images: [{ id: imageID, tags: ['demo:1', 'demo:stable'] }], containers, truncated } });
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });
it('previews fresh inventory and confirms immutable identity before a CSRF-protected removal', async () => {
 document.cookie = 'ky_csrf=image-token';
 const prompt = vi.spyOn(window, 'prompt').mockReturnValue(imageID);
 const fetcher = vi.fn(async (url: RequestInfo | URL, _init?: RequestInit) => String(url).endsWith('/inventory') ? json(inventory()) : json({ id: 'command', action: 'image.remove', outcome: 'succeeded' }, 202));
 vi.stubGlobal('fetch', fetcher);
 render(<ImageControls {...props} kind="remove" imageID={imageID} />);
 fireEvent.click(screen.getByRole('button', { name: 'Remove image' }));
 expect(await screen.findByText(/image.remove: succeeded/)).toBeTruthy();
 expect(prompt).toHaveBeenCalledWith(expect.stringContaining('Team / Production / Host'));
 expect(prompt).toHaveBeenCalledWith(expect.stringContaining('demo:1, demo:stable'));
 const call = fetcher.mock.calls[1];
 expect(call?.[0]).toBe(`${base}/commands`);
 expect(JSON.parse(String(call?.[1]?.body))).toEqual({ action: 'image.remove', reference: imageID, confirm: imageID });
 expect(new Headers(call?.[1]?.headers).get('X-CSRF-Token')).toBe('image-token');
});
it.each([
 ['dependent container', () => inventory([{ image_id: imageID, state: 'exited' }]), /used by 1 container/],
 ['truncated containers', () => inventory([], ['containers']), /incomplete/],
 ['stale report', () => ({ ...inventory(), received_at: new Date(Date.now() - 240000).toISOString() }), /stale/],
 ['missing image', () => ({ ...inventory(), snapshot: { images: [], containers: [] } }), /no longer in inventory/],
])('does not dispatch removal with a %s', async (_name, data, error) => {
 const fetcher = vi.fn(async () => json(data())); vi.stubGlobal('fetch', fetcher);
 const prompt = vi.spyOn(window, 'prompt');
 render(<ImageControls {...props} kind="remove" imageID={imageID} />);
 fireEvent.click(screen.getByRole('button', { name: 'Remove image' }));
 expect(await screen.findByText(error)).toBeTruthy();
 expect(fetcher).toHaveBeenCalledTimes(1); expect(prompt).not.toHaveBeenCalled();
});
it('does not dispatch if confirmation is cancelled or uses a tag instead of the full ID', async () => {
 const fetcher = vi.fn(async () => json(inventory())); vi.stubGlobal('fetch', fetcher);
 vi.spyOn(window, 'prompt').mockReturnValue('demo:1');
 render(<ImageControls {...props} kind="remove" imageID={imageID} />);
 fireEvent.click(screen.getByRole('button', { name: 'Remove image' }));
 await waitFor(() => expect(window.prompt).toHaveBeenCalled());
 expect(fetcher).toHaveBeenCalledTimes(1);
});
it('requires an explicit pull tag and leaves an uncertain submission blocked without retry', async () => {
 const fetcher = vi.fn(async () => { throw new Error('disconnected'); }); vi.stubGlobal('fetch', fetcher);
 vi.spyOn(window, 'confirm').mockReturnValue(true);
 render(<ImageControls {...props} kind="pull" />);
 fireEvent.change(screen.getByLabelText('Image reference'), { target: { value: 'registry.test:5000/app' } });
 fireEvent.click(screen.getByRole('button', { name: 'Pull image' }));
 expect(await screen.findByText(/explicit tag or digest/)).toBeTruthy(); expect(fetcher).not.toHaveBeenCalled();
 fireEvent.change(screen.getByLabelText('Image reference'), { target: { value: 'registry.test:5000/app:1' } });
 fireEvent.click(screen.getByRole('button', { name: 'Pull image' }));
 expect(await screen.findByText(/action may have run/)).toBeTruthy();
 fireEvent.click(screen.getByRole('button', { name: 'Pull image' })); expect(fetcher).toHaveBeenCalledTimes(1);
 expect(screen.getByRole('button', { name: 'Pull image' }).hasAttribute('disabled')).toBe(true);
});
it('polls the recorded command, displays its failure and does not resubmit', async () => {
 const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => json({ id: 'cmd', action: 'image.pull', outcome: init?.method === 'POST' ? '' : 'failed', detail: 'registry unavailable' }, init?.method === 'POST' ? 202 : 200));
 vi.stubGlobal('fetch', fetcher); vi.spyOn(window, 'confirm').mockReturnValue(true);
 render(<ImageControls {...props} kind="pull" />);
 fireEvent.change(screen.getByLabelText('Image reference'), { target: { value: 'demo:1' } });
 fireEvent.click(screen.getByRole('button', { name: 'Pull image' }));
 expect(await screen.findByText(/image.pull: failed/, {}, { timeout: 3000 })).toBeTruthy();
 expect(fetcher.mock.calls.filter((c) => c[1]?.method === 'POST')).toHaveLength(1);
 expect(fetcher.mock.calls[1]?.[0]).toBe(`${base}/commands/cmd`);
});
it('aborts a pending removal preview on unmount without dispatching afterward', async () => {
 let release: (value: Response) => void = () => {};
 const fetcher = vi.fn((_url: RequestInfo | URL, _init?: RequestInit) => new Promise<Response>((resolve) => { release = resolve; })); vi.stubGlobal('fetch', fetcher);
 const prompt = vi.spyOn(window, 'prompt');
 const view = render(<ImageControls {...props} kind="remove" imageID={imageID} />);
 fireEvent.click(screen.getByRole('button', { name: 'Remove image' })); view.unmount();
 expect(fetcher.mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
 release(json(inventory())); await Promise.resolve(); await Promise.resolve();
 expect(prompt).not.toHaveBeenCalled(); expect(fetcher).toHaveBeenCalledTimes(1);
});
