import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { applyTheme, getStoredTheme, themeNames } from '../theme';
afterEach(() => { cleanup(); localStorage.clear(); vi.unstubAllGlobals(); });
it('shows the fifteen KyPost/KyDNS swatches and keeps theme choice browser-local', () => {
  const fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
  render(<><ThemeSwitcher swatches /><ThemeSwitcher /></>);
  expect(screen.getAllByRole('button')).toHaveLength(15);
  expect(themeNames).toContain('Polished Ky');
  fireEvent.click(screen.getByRole('button', { name: 'Ocean' }));
  expect(getStoredTheme()).toBe('Ocean');
  expect(document.documentElement.style.getPropertyValue('--bg')).toBe('#0f1b24');
  expect(screen.getByRole('combobox')).toHaveProperty('value', 'Ocean');
  expect(fetcher).not.toHaveBeenCalled();
  applyTheme('Polished Ky');
  expect(document.documentElement.style.colorScheme).toBe('light');
});
