import { describe, expect, it, vi } from 'vitest';
import { reloadOnStaleChunk } from './staleChunk';

describe('reloadOnStaleChunk', () => {
  it('reloads once a minute when a lazy chunk fails to load', () => {
    const target = new EventTarget();
    const reload = vi.fn();
    let clock = 1_000_000;
    sessionStorage.clear();
    reloadOnStaleChunk(target, reload, () => sessionStorage, () => clock);
    const failed = () => { const e = new Event('vite:preloadError', { cancelable: true }); target.dispatchEvent(e); return e; };

    expect(failed().defaultPrevented).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
    clock += 30_000;
    expect(failed().defaultPrevented).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
    clock += 31_000;
    failed();
    expect(reload).toHaveBeenCalledTimes(2);
  });

  it('still reloads when storage is blocked', () => {
    const target = new EventTarget();
    const reload = vi.fn();
    reloadOnStaleChunk(target, reload, () => { throw new DOMException('denied', 'SecurityError'); });
    const e = new Event('vite:preloadError', { cancelable: true });
    target.dispatchEvent(e);
    expect(e.defaultPrevented).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
  });
});
