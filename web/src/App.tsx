import React, { useEffect, useState } from 'react';
import { AppHeader } from './components/AppHeader';
import { Dashboard } from './pages/Dashboard';
import { Login } from './pages/Login';
import { ChangePassword } from './pages/ChangePassword';
import { Backup } from './pages/Backup';
import { Settings } from './pages/Settings';
import { Organization } from './pages/Organization';
import { Members } from './pages/Members';
import { Environment } from './pages/Environment';
import { EndpointPage } from './pages/EndpointPage';
import { AuditList } from './components/AuditList';
import { Link } from './components/Link';
import './styles/theme.css';
import { secureFetch } from './api';
import { useRoute, type Route } from './router';

export const App: React.FC = () => {
  const [user, setUser] = useState<any>(null);
  const [notice, setNotice] = useState('');
  const [loading, setLoading] = useState<boolean>(true);
  const [settings, setSettings] = useState<any>(null);
  const route = useRoute();

  useEffect(() => {
    const checkAuth = async () => {
      try {
        const authResp = await fetch('/api/auth/me');
        const setResp = await fetch('/api/settings');
        if (setResp.ok) {
          const s = await setResp.json();
          setSettings(s);
        }
        if (authResp.ok) {
          const a = await authResp.json();
          if (a.authenticated) setUser(a.user);
        }
      } catch (err) {
        console.error('Initialization error:', err);
      } finally {
        setLoading(false);
      }
    };
    checkAuth();
  }, []);

  // /api/settings returns more fields once authenticated, so re-read it after login.
  const loadSettings = async () => {
    const resp = await fetch('/api/settings');
    if (resp.ok) {
      const s = await resp.json();
      setSettings(s);
    }
  };

  const handleLogout = async () => {
    await secureFetch('/api/auth/logout', { method: 'POST' });
    setUser(null);
  };

  if (loading) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: 'var(--bg)', color: 'var(--ink)' }}>
        Loading {settings?.app_name || 'KyYard'}...
      </div>
    );
  }

  // The requested path is kept while signed out, so a deep link opens after sign-in.
  if (!user) {
    return (
      <>
      {notice && <p role="status" style={{ padding: 16 }}>{notice}</p>}
      <Login
        appName={settings?.app_name || 'KyYard'}
        providers={settings?.sso_providers ?? []}
        appURL={typeof settings?.app_url === 'string' ? settings.app_url : ''}
        onSuccess={(u) => {
          setNotice('');
          setUser(u);
          void loadSettings();
        }}
      />
      </>
    );
  }

  if (user.must_change_password) {
    return <ChangePassword onLogout={handleLogout} onComplete={() => {
      setUser(null);
      setNotice('Password changed. Sign in with your new password.');
    }} />;
  }

  const org = 'org' in route ? route.org : undefined;
  return (
    <div className="ky-shell">
      <a className="ky-skip" href="#main-content">Skip to content</a>
      <AppHeader appName={settings?.app_name || 'KyYard'} route={route} user={user} onLogout={handleLogout} />
      {/* Keying on the organization discards every tenant screen's state when the context changes. */}
      <main id="main-content" className="ky-main" tabIndex={-1} key={org ?? ''}>
        <Screen route={route} settings={settings} user={user} />
      </main>
    </div>
  );
};

const Screen: React.FC<{ route: Route; settings: any; user: any }> = ({ route, settings }) => {
  switch (route.name) {
    case 'dashboard': return <Dashboard />;
    case 'endpoints': return <Dashboard mode="endpoints" />;
    case 'backup': return <Backup />;
    case 'settings': return <Settings settings={settings} />;
    case 'organization': return <Organization org={route.org} />;
    case 'members': return <Members org={route.org} />;
    case 'environment': return <Environment key={`${route.org}/${route.env}`} org={route.org} env={route.env} />;
    case 'endpoint': return <EndpointPage key={`${route.org}/${route.endpoint}`} org={route.org} endpoint={route.endpoint} />;
    case 'audit': return (
      <div className="ky-page">
        <h1 style={{ fontSize: 24 }}>Audit history</h1>
        <nav aria-label="Administration" className="ky-subnav"><Link to={`/organizations/${encodeURIComponent(route.org)}`}>Environments</Link></nav>
        <AuditList url={`/api/organizations/${encodeURIComponent(route.org)}/audit`} />
      </div>
    );
    default: return (
      <div className="ky-page" role="alert">
        <h1 style={{ fontSize: 24 }}>Page not found</h1>
        <p><Link to="/">Return to the overview</Link></p>
      </div>
    );
  }
};
