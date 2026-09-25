import { useEffect, useState } from 'react';
import { THEME_OPTIONS, applyTheme, storedTheme } from '../theme';

export function ThemeSwitcher() {
  const [theme, setTheme] = useState(storedTheme);
  useEffect(() => {
    const sync = () => setTheme(storedTheme());
    window.addEventListener('storage', sync);
    return () => window.removeEventListener('storage', sync);
  }, []);
  return <select className="theme-selector" aria-label="Color theme" value={theme} onChange={event => {
    setTheme(event.target.value);
    applyTheme(event.target.value, true);
  }}>{THEME_OPTIONS.map(option => <option key={option.id} value={option.id}>{option.label}</option>)}</select>;
}
