import { ThemeSwitcher } from '../components/ThemeSwitcher';
import { Link } from '../components/Link';
import { SSOSettings } from '../components/SSOSettings';

export function Settings({ settings }: { settings: { db_driver?: string } | null }) {
  return <div className="ky-page">
    <h1>Settings</h1>
    <section className="panel"><h2>Appearance</h2><p>Pick a theme. It applies straight away and is remembered by this browser only.</p><ThemeSwitcher swatches /></section>
    <SSOSettings />
    <section className="panel"><h2>Recovery</h2><p>Back up the control plane and verify recovery.</p><Link to="/backup">Open backup & recovery</Link></section>
    <section className="panel"><h2>Storage</h2><p>Database: {settings?.db_driver ?? 'unavailable'}</p></section>
  </div>;
}
