import { useEffect, useState } from 'react';
import { applyTheme, getStoredTheme, isThemeName, themeNames, themes } from '../theme';

export function ThemeSwitcher({ swatches = false }: { swatches?: boolean }) {
  const [selected, setSelected] = useState(getStoredTheme);
  useEffect(() => {
    const update = () => { const value = document.documentElement.dataset.theme ?? ''; if (isThemeName(value)) setSelected(value); };
    window.addEventListener('ky:theme', update);
    return () => window.removeEventListener('ky:theme', update);
  }, []);
  const choose = (name: string) => { if (isThemeName(name)) { setSelected(name); applyTheme(name); } };
  if (!swatches) return <select aria-label="Select color theme" value={selected} onChange={(e) => choose(e.target.value)} style={{ width: 'auto', maxWidth: 180 }}>{themeNames.map((name) => <option key={name}>{name}</option>)}</select>;
  return <div className="theme-grid">{themeNames.map((name) => {
    const c = themes[name];
    return <button key={name} type="button" aria-pressed={name === selected} className="theme-swatch" onClick={() => choose(name)}>
      <span className="theme-swatch-preview" style={{ background: `linear-gradient(90deg, ${c.sidebarStart} 30%, ${c.bg} 30%)` }}><i style={{ background: c.accent }} /><i style={{ background: c.inkStrong, opacity: 0.55 }} /></span>
      <span className="theme-swatch-name">{name}</span>
    </button>;
  })}</div>;
}
