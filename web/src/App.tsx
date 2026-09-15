import React, { useEffect, useState } from 'react';
import { AppHeader } from './components/AppHeader';
import { Dashboard } from './pages/Dashboard';
import { Login } from './pages/Login';
import { Backup } from './pages/Backup';
import { SCIMAdmin } from './pages/SCIMAdmin';
import { Settings } from './pages/Settings';
import './styles/theme.css';
import { secureFetch } from './api';

export const App: React.FC = () => {
  const [user, setUser] = useState<any>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [activeTab, setActiveTab] = useState<string>('dashboard');
  const [settings, setSettings] = useState<any>(null);

  useEffect(() => {
    const checkAuth = async () => {
      try {
        const [authResp, setResp] = useResponses(
          await fetch('/api/auth/me'),
          await fetch('/api/settings')
        );

        if (setResp.ok) {
          const s = await setResp.json();
          setSettings(s);
          if (s.theme) {
            document.documentElement.setAttribute('data-theme', s.theme);
          }
        }

        if (authResp.ok) {
          const a = await authResp.json();
          if (a.authenticated) {
            setUser(a.user);
          }
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
      if (s.theme) {
        document.documentElement.setAttribute('data-theme', s.theme);
      }
    }
  };

  const handleLogout = async () => {
    await secureFetch('/api/auth/logout', { method: 'POST' });
    setUser(null);
  };

  if (loading) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: 'var(--bg)', color: 'var(--ink)' }}>
        Loading {settings?.app_name || 'Busnes.app'}...
      </div>
    );
  }

  if (!user) {
    return (
      <Login
        appName={settings?.app_name || 'Busnes.app'}
        onSuccess={(u) => {
          setUser(u);
          void loadSettings();
        }}
      />
    );
  }

  return (
    <div style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      <AppHeader
        appName={settings?.app_name || 'Busnes.app'}
        activeTab={activeTab}
        onTabChange={(tab) => setActiveTab(tab)}
        user={user}
        onLogout={handleLogout}
      />

      <main style={{ flex: 1 }}>
        {activeTab === 'dashboard' && <Dashboard settings={settings} user={user} onNavigate={(tab) => setActiveTab(tab)} />}
        {activeTab === 'scim' && <SCIMAdmin />}
        {activeTab === 'backup' && <Backup />}
        {activeTab === 'settings' && <Settings settings={settings} />}
      </main>
    </div>
  );
};

function useResponses(r1: Response, r2: Response): [Response, Response] {
  return [r1, r2];
}
