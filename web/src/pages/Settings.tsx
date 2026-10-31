import { useState } from 'react';
import { Link } from '../components/Link';
import { AccountSSO } from '../components/AccountSSO';
import { AccountSecurity } from '../components/AccountSecurity';
import { SSOSettings } from '../components/SSOSettings';
import { Administration } from '../components/Administration';

// role is the platform role from /api/auth/me; the server refuses /api/admin/* to anyone else regardless.
export function Settings({ settings, role }: { settings: { db_driver?: string } | null; role?: string }) {
  const sections = ['security', 'sign-in', 'recovery', 'storage', ...(role === 'admin' ? ['suite', 'administration'] : [])];
  const query = new URLSearchParams(window.location.search);
  // ?section= deep-links a tab (the empty Home page links Administration); unknown names fall back.
  const [section, setSection] = useState(query.get('sso') === 'linked' ? 'sign-in' : sections.includes(query.get('section') ?? '') ? query.get('section')! : 'security');
  return <div className="ky-page">
    <header className="ky-page-head">
      <div>
        <h1 className="ky-page-title">Settings</h1>
        <p className="ky-page-lede">Account security, sign-in, recovery and storage for this installation.</p>
      </div>
    </header>
    <nav className="ky-resource-tabs" aria-label="Settings sections">
      {sections.map((name) => <button key={name} aria-pressed={section === name} onClick={() => setSection(name)}>{name === 'sign-in' ? 'Sign-in' : name[0].toUpperCase() + name.slice(1)}</button>)}
    </nav>
    {section === 'security' && <AccountSecurity />}
    {section === 'sign-in' && <><AccountSSO />{role === 'admin' && <SSOSettings />}</>}
    {section === 'recovery' && <section className="panel"><h2>Backup & recovery</h2><p>Back up the control plane and verify that it can be restored.</p><Link className="btn btn-secondary" to="/backup">Open backup & recovery</Link></section>}
    {section === 'suite' && role === 'admin' && <section className="panel"><h2>Suite</h2><p>Install, upgrade and remove Ky suite apps through the suite runner KyQuickStart hands off to KyYard.</p><Link className="btn btn-secondary" to="/suite">Open suite</Link></section>}
    {section === 'administration' && role === 'admin' && <Administration />}
    {section === 'storage' && <section className="panel"><h2>Storage</h2><p>Database: {settings?.db_driver?.toUpperCase() ?? 'unavailable'}</p></section>}
  </div>;
}
