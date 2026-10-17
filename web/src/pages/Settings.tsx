import { useState } from 'react';
import { ThemeSwitcher } from '../components/ThemeSwitcher';
import { Link } from '../components/Link';
import { AccountSSO } from '../components/AccountSSO';
import { AccountSecurity } from '../components/AccountSecurity';
import { SSOSettings } from '../components/SSOSettings';
import { Administration } from '../components/Administration';

// role is the platform role from /api/auth/me; the server refuses /api/admin/* to anyone else regardless.
export function Settings({ settings, role }: { settings: { db_driver?: string } | null; role?: string }) {
  const [section, setSection] = useState(new URLSearchParams(window.location.search).get('sso') === 'linked' ? 'sign-in' : 'appearance');
  const sections = ['appearance', 'security', 'sign-in', 'recovery', 'storage', ...(role === 'admin' ? ['suite', 'administration'] : [])];
  return <div className="ky-page">
    <h1>Settings</h1>
    <p>Appearance, account security, sign-in, and recovery for this installation.</p>
    <nav className="ky-resource-tabs" aria-label="Settings sections">
      {sections.map((name) => <button key={name} aria-pressed={section === name} onClick={() => setSection(name)}>{name === 'sign-in' ? 'Sign-in' : name[0].toUpperCase() + name.slice(1)}</button>)}
    </nav>
    {section === 'appearance' && <section className="panel"><h2>Appearance</h2><p>Choose a Ky theme. Your preference stays in this browser.</p><ThemeSwitcher swatches /></section>}
    {section === 'security' && <AccountSecurity />}
    {section === 'sign-in' && <><AccountSSO />{role === 'admin' && <SSOSettings />}</>}
    {section === 'recovery' && <section className="panel"><h2>Backup & recovery</h2><p>Back up the control plane and verify that it can be restored.</p><Link className="btn btn-secondary" to="/backup">Open backup & recovery</Link></section>}
    {section === 'suite' && role === 'admin' && <section className="panel"><h2>Suite</h2><p>Install, upgrade and remove Ky suite apps through the suite runner KyQuickStart hands off to KyYard.</p><Link className="btn btn-secondary" to="/suite">Open suite</Link></section>}
    {section === 'administration' && role === 'admin' && <Administration />}
    {section === 'storage' && <section className="panel"><h2>Storage</h2><p>Database: {settings?.db_driver?.toUpperCase() ?? 'unavailable'}</p></section>}
  </div>;
}
