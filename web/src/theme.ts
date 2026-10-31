import { readChoice, saveChoice, watchChoice } from './ky-ui/theme';
import { SYSTEM_THEME, THEME_PAIRS, applyThemeVariables, findPair, findTheme } from './ky-ui/themes';

// The saved value is `system` (Busnes following the OS), a pair ID (that pair following the OS) or
// a half ID (a fixed mode). Names and aliases saved by earlier versions resolve to their half;
// removed palettes fall back to `system`.
const key = 'kyyard-theme';
export const THEME_CHOICES = THEME_PAIRS.map((pair) => ({
  pair,
  choices: [
    { mode: 'system', label: 'Match system', value: pair.id === 'busnes-pair' ? SYSTEM_THEME.id : pair.id },
    { mode: 'light', label: 'Light', value: pair.light },
    { mode: 'dark', label: 'Dark', value: pair.dark },
  ],
}));

export function storedTheme(): string {
  const saved = readChoice(key);
  return findPair(saved)?.id ?? findTheme(saved)?.id ?? SYSTEM_THEME.id;
}

const media = window.matchMedia?.('(prefers-color-scheme: dark)');
export function applyTheme(theme: string, persist = false) {
  const option = findTheme(theme);
  if (!option) return;
  const root = document.documentElement;
  const resolved = option.id === 'system' ? (media?.matches ? 'busnes-dark' : 'busnes-light') : option.id;
  const scheme = applyThemeVariables(resolved) ?? (resolved === 'busnes-light' ? 'light' : 'dark');
  root.dataset.theme = resolved;
  root.dataset.scheme = scheme;
  root.style.colorScheme = scheme;
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', getComputedStyle(root).getPropertyValue('--bg').trim());
  if (persist) saveChoice(key, findPair(theme)?.id ?? option.id);
  window.dispatchEvent(new Event('ky:theme'));
}

applyTheme(storedTheme());
watchChoice(key, () => applyTheme(storedTheme()));
