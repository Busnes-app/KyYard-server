import React, { useEffect, useState } from 'react';
import { Palette } from 'lucide-react';
import { secureFetch } from '../api';

const THEMES = [
  { id: 'patina', label: 'Patina Ky (Default)' },
  { id: 'cyber', label: 'Cyber Dark' },
  { id: 'nord', label: 'Nord Slate' },
  { id: 'paper', label: 'Paper Clean' },
  { id: 'oled', label: 'OLED Black' },
];

export const ThemeSwitcher: React.FC = () => {
  const [currentTheme, setCurrentTheme] = useState<string>('patina');

  useEffect(() => {
    const saved = localStorage.getItem('ky_theme') || 'patina';
    setCurrentTheme(saved);
    document.documentElement.setAttribute('data-theme', saved);
  }, []);

  const switchTheme = (theme: string) => {
    setCurrentTheme(theme);
    localStorage.setItem('ky_theme', theme);
    document.documentElement.setAttribute('data-theme', theme);
    // Optionally persist to backend
    secureFetch('/api/settings/theme', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ theme }),
    }).catch(() => {});
  };

  return (
    <div style={{ display: 'inline-flex', alignItems: 'center', gap: '6px' }}>
      <Palette size={16} style={{ color: 'var(--ink)' }} />
      <select
        value={currentTheme}
        onChange={(e) => switchTheme(e.target.value)}
        style={{
          width: 'auto',
          padding: '4px 8px',
          fontSize: '12px',
          background: 'var(--panel)',
          color: 'var(--ink-strong)',
          borderColor: 'var(--line)',
        }}
        aria-label="Select color theme"
      >
        {THEMES.map((t) => (
          <option key={t.id} value={t.id}>
            {t.label}
          </option>
        ))}
      </select>
    </div>
  );
};
