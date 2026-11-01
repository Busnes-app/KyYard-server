import { Link } from '../components/Link';
import { AccountSSO } from '../components/AccountSSO';
import { AccountSecurity } from '../components/AccountSecurity';
import { SSOSettings } from '../components/SSOSettings';
import { Administration } from '../components/Administration';
import { Appearance } from './Appearance';
import { settingsPath, settingsSections, type SettingsSection } from '../router';

// role is the platform role from /api/auth/me; the server refuses /api/admin/* to anyone else regardless.
// One tab per section, each its own path, so links, reload and Back work; the open tab mounts alone,
// so provider configuration loads only when Sign-in is open.
export function Settings({ section, settings, role }: { section: SettingsSection; settings: { db_driver?: string } | null; role?: string }) {
  const sections = settingsSections(role);
  const open = sections.find((s) => s.id === section) ?? sections[0];
  return <div className="ky-page">
    <header className="ky-page-head">
      <div>
        <h1 className="ky-page-title">Settings</h1>
        <p className="ky-page-lede">Your account, how KyYard looks, and recovery and storage for this installation.</p>
      </div>
    </header>
    <nav className="ky-settings-tabs" aria-label="Settings sections">
      {sections.map((s) => <Link key={s.id} to={settingsPath(s.id)} current={s === open} className="ky-nav-item">{s.label}</Link>)}
    </nav>
    <section aria-label={open.label} className="settings-section">
      {open.id === 'account' && <><AccountSecurity /><AccountSSO /></>}
      {open.id === 'appearance' && <Appearance />}
      {open.id === 'sign-in' && <SSOSettings />}
      {open.id === 'recovery' && <section className="panel"><h2>Backup & recovery</h2><p>Back up the control plane and verify that it can be restored.</p><Link className="btn btn-secondary" to="/backup">Open backup & recovery</Link></section>}
      {open.id === 'storage' && <section className="panel"><h2>Storage</h2><p>Database: {settings?.db_driver?.toUpperCase() ?? 'unavailable'}</p></section>}
      {open.id === 'suite' && <section className="panel"><h2>Suite</h2><p>Install, upgrade and remove Ky suite apps through the suite runner KyQuickStart hands off to KyYard.</p><Link className="btn btn-secondary" to="/suite">Open suite</Link></section>}
      {open.id === 'administration' && <Administration />}
    </section>
  </div>;
}
