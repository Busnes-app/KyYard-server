import { useEffect, useState } from 'react';
import { applyTheme, getStoredTheme, themes } from '../theme';

const mode = (scheme: string) => scheme === 'system' ? 'Follows device' : scheme === 'light' ? 'Light' : 'Dark';

/** The shared ky-ui Appearance cards: native radios, applied and saved on change. */
export function ThemeSwitcher() {
  const [selected, setSelected] = useState(getStoredTheme);
  useEffect(() => {
    const update = () => setSelected(getStoredTheme());
    window.addEventListener('ky:theme', update);
    return () => window.removeEventListener('ky:theme', update);
  }, []);
  return <fieldset className="ky-appearance">
    <legend className="ky-appearance-legend">Theme</legend>
    <div className="ky-appearance-grid">
      {themes.map((theme) => <label key={theme.id} className="ky-appearance-option">
        <input className="ky-appearance-radio" type="radio" name="kyyard-theme" value={theme.id} checked={theme.id === selected.id} onChange={() => { setSelected(theme); applyTheme(theme); }} />
        <span className="ky-appearance-preview" aria-hidden="true" data-ky-preview={theme.id} />
        <span className="ky-appearance-name">{theme.name}</span>
        <small className="ky-appearance-mode">{mode(theme.scheme)}</small>
      </label>)}
    </div>
  </fieldset>;
}
