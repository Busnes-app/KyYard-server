import { readChoice, saveChoice } from './ky-ui/theme';
import { SYSTEM_THEME, THEME_CATALOG, applyThemeVariables, findTheme } from './ky-ui/themes';
import type { CatalogTheme, SystemTheme } from './ky-ui/themes';

// The suite catalog: Match system, Busnes Light/Dark and every named palette.
export type Theme = CatalogTheme | SystemTheme;
export const themes: readonly Theme[] = [SYSTEM_THEME, ...THEME_CATALOG];
const key = 'kyyard-theme';
const prefersDark = () => window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false;

/** The saved choice (names saved by earlier versions included), else Match system. */
export function getStoredTheme(): Theme {
  try { return findTheme(readChoice(key)) ?? SYSTEM_THEME; } catch { return SYSTEM_THEME; }
}

export function applyTheme(theme: Theme, persist = true) {
  const root = document.documentElement;
  const named = applyThemeVariables(theme.id, root);
  if (named) {
    // data-theme keeps the base-family rules (patina, cyber, nord, paper, oled) for the
    // variables the shared palette does not set; the shared ones are inline and win.
    root.dataset.theme = theme.id;
    delete root.dataset.kyTheme;
    root.style.setProperty('--danger', named === 'light' ? '#b91c1c' : '#f87171');
    root.style.setProperty('--success', named === 'light' ? '#166534' : '#34d399');
  } else {
    const busnes = theme.id === 'system' ? (prefersDark() ? 'busnes-dark' : 'busnes-light') : theme.id;
    root.dataset.theme = busnes;
    root.dataset.kyTheme = busnes;
    root.style.colorScheme = busnes === 'busnes-dark' ? 'dark' : 'light';
    root.style.removeProperty('--danger');
    root.style.removeProperty('--success');
  }
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', getComputedStyle(root).getPropertyValue('--bg').trim());
  // Names are what earlier versions saved; keep writing them.
  if (persist) { try { saveChoice(key, theme.id === 'system' ? 'system' : theme.name); } catch { /* Applies without storage. */ } }
  window.dispatchEvent(new Event('ky:theme'));
}

window.addEventListener('storage', (e) => { if (e.key === key) applyTheme(getStoredTheme(), false); });
window.matchMedia?.('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (getStoredTheme().id === 'system') applyTheme(SYSTEM_THEME, false);
});
