const KEY = 'kyyard-stale-chunk-reload';

// A tab opened before an upgrade asks for a lazy chunk (a Terminal) the new build no longer has,
// and the failed import would blank the whole app. Reload into the current build instead, at most
// once a minute, so a build that really lacks the chunk cannot loop.
export function reloadOnStaleChunk(target: EventTarget, reload: () => void, storage: Storage, now = Date.now): void {
  target.addEventListener('vite:preloadError', (event) => {
    if (now() - Number(storage.getItem(KEY) ?? 0) < 60_000) return;
    storage.setItem(KEY, String(now()));
    event.preventDefault();
    reload();
  });
}
