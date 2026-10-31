import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ThemeSwitcher } from './ThemeSwitcher';
import { Appearance } from '../pages/Appearance';
import { THEME_CHOICES, storedTheme } from '../theme';
import { THEME_PAIRS, THEME_PALETTES } from '../ky-ui/themes';
afterEach(() => { cleanup(); localStorage.clear(); document.documentElement.removeAttribute('style'); vi.unstubAllGlobals(); });

it('offers each pair with Match system, Light and Dark as one radio group, browser-local', () => {
  const fetcher = vi.fn(); vi.stubGlobal('fetch', fetcher);
  render(<Appearance />);
  expect(screen.getAllByRole('radio')).toHaveLength(THEME_PAIRS.length * 3);
  const busnes = within(screen.getByRole('group', { name: 'Busnes' }));
  expect(busnes.getByRole('radio', { name: 'Match system' })).toHaveProperty('checked', true);
  fireEvent.click(within(screen.getByRole('group', { name: 'Ocean' })).getByRole('radio', { name: 'Dark' }));
  expect(localStorage.getItem('kyyard-theme')).toBe('ocean');
  expect(storedTheme()).toBe('ocean');
  expect(document.documentElement.style.getPropertyValue('--bg')).toBe('#0f1b24');
  expect(document.documentElement.dataset.theme).toBe('ocean');
  expect(document.documentElement.dataset.scheme).toBe('dark');
  fireEvent.click(busnes.getByRole('radio', { name: 'Dark' }));
  expect(document.documentElement.style.getPropertyValue('--bg')).toBe('');
  expect(document.documentElement.dataset.theme).toBe('busnes-dark');
  expect(document.documentElement.style.colorScheme).toBe('dark');
  fireEvent.click(busnes.getByRole('radio', { name: 'Match system' }));
  expect(storedTheme()).toBe('system');
  expect(fetcher).not.toHaveBeenCalled();
});

it('selects choices saved by earlier versions, by name or ID', () => {
  localStorage.setItem('kyyard-theme', 'Polished Ky');
  render(<Appearance />);
  expect(within(screen.getByRole('group', { name: 'Ky' })).getByRole('radio', { name: 'Light' })).toHaveProperty('checked', true);
  cleanup();
  localStorage.setItem('kyyard-theme', 'patina-ky');
  render(<ThemeSwitcher />);
  expect((screen.getByRole('combobox', { name: 'Color theme' }) as HTMLSelectElement).value).toBe('patina-ky');
});

it('the sign-in picker lists the same choices', () => {
  render(<ThemeSwitcher />);
  expect(screen.getAllByRole('option')).toHaveLength(THEME_CHOICES.length * 3);
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
