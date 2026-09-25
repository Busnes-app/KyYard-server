import { useState } from 'react';
import { ThemeSwitcher } from '../components/ThemeSwitcher';
import { Link } from '../components/Link';
import { SSOSettings } from '../components/SSOSettings';
import { Administration } from '../components/Administration';

// role is the platform role from /api/auth/me; the server refuses /api/admin/* to anyone else regardless.
export function Settings({ settings, role }: { settings: { db_driver?: string } | null; role?: string }) {
  const [section, setSection] = useState('appearance');
  const sections = ['appearance', 'sign-in', 'recovery', 'storage', ...(role === 'admin' ? ['administration'] : [])];
  return <div className="ky-page">
    <h1>Settings</h1>
    <p>Appearance, sign-in, and recovery for this installation.</p>
    <nav className="ky-resource-tabs" aria-label="Settings sections">
      {sections.map((name) => <button key={name} aria-pressed={section === name} onClick={() => setSection(name)}>{name === 'sign-in' ? 'Sign-in' : name[0].toUpperCase() + name.slice(1)}</button>)}
    </nav>
    {section === 'appearance' && <section className="panel"><h2>Appearance</h2><p>Choose a Ky theme. Your preference stays in this browser.</p><ThemeSwitcher swatches /></section>}
    {section === 'sign-in' && <SSOSettings />}
    {section === 'recovery' && <section className="panel"><h2>Backup & recovery</h2><p>Back up the control plane and verify that it can be restored.</p><Link className="btn btn-secondary" to="/backup">Open backup & recovery</Link></section>}
    {section === 'administration' && role === 'admin' && <Administration />}
    {section === 'storage' && <section className="panel"><h2>Storage</h2><p>Database: {settings?.db_driver?.toUpperCase() ?? 'unavailable'}</p></section>}
  </div>;
}
