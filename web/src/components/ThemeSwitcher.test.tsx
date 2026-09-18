import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { applyTheme, getStoredTheme, themeNames, themes } from '../theme';
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

it('keeps body, panel and primary-button text at AA contrast in every palette', () => {
  const luminance = (hex: string) => {
    const values = [1, 3, 5].map((start) => Number.parseInt(hex.slice(start, start + 2), 16) / 255).map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
    return (values[0] ?? 0) * 0.2126 + (values[1] ?? 0) * 0.7152 + (values[2] ?? 0) * 0.0722;
  };
  for (const theme of Object.values(themes)) {
    for (const [foreground, background] of [[theme.ink, theme.bg], [theme.ink, theme.panel], [theme.inkStrong, theme.panel], [theme.buttonText, theme.accent]]) {
      const a = luminance(foreground), b = luminance(background);
      expect((Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)).toBeGreaterThanOrEqual(4.5);
    }
  }
});
