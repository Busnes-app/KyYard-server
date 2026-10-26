import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { getStoredTheme } from '../theme';
import { THEME_PALETTES } from '../ky-ui/themes';
afterEach(() => { cleanup(); localStorage.clear(); document.documentElement.removeAttribute('style'); vi.unstubAllGlobals(); });

it('offers Match system and the suite catalog as radios and keeps the choice browser-local', () => {
  const fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
  render(<ThemeSwitcher />);
  const radios = screen.getAllByRole('radio');
  expect(radios).toHaveLength(22);
  expect(screen.getByRole('radio', { name: /Match system/ })).toHaveProperty('checked', true);
  fireEvent.click(screen.getByRole('radio', { name: /^Ocean/ }));
  expect(localStorage.getItem('kyyard-theme')).toContain('Ocean');
  expect(getStoredTheme().id).toBe('ocean');
  expect(document.documentElement.style.getPropertyValue('--bg')).toBe('#0f1b24');
  expect(document.documentElement.dataset.theme).toBe('ocean');
  fireEvent.click(screen.getByRole('radio', { name: /^Busnes Dark/ }));
  expect(document.documentElement.style.getPropertyValue('--bg')).toBe('');
  expect(document.documentElement.dataset.theme).toBe('busnes-dark');
  expect(document.documentElement.style.colorScheme).toBe('dark');
  fireEvent.click(screen.getByRole('radio', { name: /Match system/ }));
  expect(getStoredTheme().id).toBe('system');
  expect(fetcher).not.toHaveBeenCalled();
});

it('selects choices saved by earlier versions', () => {
  localStorage.setItem('kyyard-theme', 'Patina Ky');
  render(<ThemeSwitcher />);
  expect(screen.getByRole('radio', { name: /^Patina Ky/ })).toHaveProperty('checked', true);
});

it('keeps body, panel and primary-button text at AA contrast in every named palette', () => {
  const luminance = (hex: string) => {
    const values = [1, 3, 5].map((start) => Number.parseInt(hex.slice(start, start + 2), 16) / 255).map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
    return (values[0] ?? 0) * 0.2126 + (values[1] ?? 0) * 0.7152 + (values[2] ?? 0) * 0.0722;
  };
  for (const theme of Object.values(THEME_PALETTES)) {
    for (const [foreground, background] of [[theme.ink, theme.bg], [theme.ink, theme.panel], [theme.inkStrong, theme.panel], [theme.buttonText, theme.accent]]) {
      const a = luminance(foreground), b = luminance(background);
      expect((Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)).toBeGreaterThanOrEqual(4.5);
    }
  }
});
