import React, { useState, useEffect } from 'react';
import { Key, AlertCircle, LogIn } from 'lucide-react';
import { CaptchaWidget } from '../components/CaptchaWidget';
import { SetupGuide } from '../components/SetupGuide';
import { ThemeSwitcher } from '../components/ThemeSwitcher';

interface LoginProps {
  onSuccess: (user: any) => void;
  appName: string;
  appURL?: string;
  providers?: { id: string; name: string; login_url: string }[];
}

interface MFAChallenge {
  mfa_token: string;
}

export const Login: React.FC<LoginProps> = ({ onSuccess, appName, appURL = '', providers = [] }) => {
  const [username, setUsername] = useState<string>('');
  const [password, setPassword] = useState<string>('');
  const [captchaToken, setCaptchaToken] = useState<string>('');
  // A solved challenge is accepted once; bumping this remounts the widget for a fresh one.
  const [captchaRound, setCaptchaRound] = useState(0);
  const [mfaChallenge, setMfaChallenge] = useState<MFAChallenge | null>(() => {
    const match = /^#sso-mfa=([a-f0-9]{64})$/.exec(window.location.hash);
    if (!match) return null;
    return { mfa_token: match[1] };
  });
  useEffect(() => {
    if (/^#sso-mfa=[a-f0-9]{64}$/.test(window.location.hash)) {
      window.history.replaceState(null, '', window.location.pathname + window.location.search);
    }
  }, []);
  const [mfaCode, setMfaCode] = useState<string>('');
  const [isRecoveryCode, setIsRecoveryCode] = useState<boolean>(false);
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setLoading(true);

    try {
      const resp = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          username,
          password,
          captcha_token: captchaToken || undefined,
        }),
      });

      const data = await resp.json();
      if (!resp.ok) {
        setCaptchaToken('');
        setCaptchaRound((n) => n + 1);
        throw new Error(data.error || 'Login failed');
      }

      if (data.mfa_required) {
        setMfaChallenge(data);
        return;
      }

      if (data.authenticated) {
        onSuccess(data.user);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Login failed');
    } finally {
      setLoading(false);
    }
  };

  const handleMFASubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!mfaChallenge) return;
    setError('');
    setLoading(true);

    try {
      const endpoint = isRecoveryCode ? '/api/auth/mfa/recovery-code' : '/api/auth/mfa/totp';
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          mfa_token: mfaChallenge.mfa_token,
          code: mfaCode.trim(),
        }),
      });

      const data = await resp.json();
      if (!resp.ok) {
        throw new Error(data.error || 'MFA verification failed');
      }

      if (data.authenticated) {
        onSuccess(data.user);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid verification code');
    } finally {
      setLoading(false);
    }
  };

  return (
    <main className="ky-auth">
      <div className="ky-auth-theme"><ThemeSwitcher /></div>
      <div className="ky-auth-body">
        <div className="ky-auth-brand">
          <img className="ky-mark" src="/app-icon.png" width={56} height={56} alt="" />
          <h1>{appName || 'KyYard'}</h1>
          <p>The control plane for your container fleet.</p>
        </div>

        <div className="ky-auth-card">
          {error && <p role="alert" className="ky-auth-error"><AlertCircle size={16} aria-hidden="true" /><span>{error}</span></p>}

          {mfaChallenge ? (
            <form onSubmit={handleMFASubmit}>
              <h2>Two-factor verification</h2>
              <p className="ky-auth-note">{isRecoveryCode ? 'Enter one of your single-use recovery codes.' : 'Enter the 6-digit code from your authenticator app.'}</p>
              <div className="ky-auth-field">
                <label htmlFor="mfa-code">{isRecoveryCode ? 'Recovery code' : 'Authenticator code'}</label>
                <input
                  id="mfa-code"
                  className="ky-code-input"
                  type="text"
                  inputMode={isRecoveryCode ? 'text' : 'numeric'}
                  autoComplete="one-time-code"
                  autoFocus
                  value={mfaCode}
                  onChange={(e) => setMfaCode(e.target.value)}
                  placeholder={isRecoveryCode ? 'XXXX-XXXX' : '123456'}
                  required
                />
              </div>
              <button type="submit" className="ky-auth-submit" disabled={loading}>{loading ? 'Verifying…' : 'Verify and sign in'}</button>
              <button type="button" className="ky-auth-link" onClick={() => setIsRecoveryCode(!isRecoveryCode)}>
                {isRecoveryCode ? 'Use your authenticator app' : 'Use a recovery code'}
              </button>
            </form>
          ) : (
            <form onSubmit={handleLogin}>
              <div className="ky-auth-field">
                <label htmlFor="login-username">Username</label>
                <input id="login-username" type="text" autoComplete="username" autoCapitalize="none" spellCheck={false} autoFocus
                  value={username} onChange={(e) => setUsername(e.target.value)} required />
              </div>
              <div className="ky-auth-field">
                <label htmlFor="login-password">Password</label>
                <input id="login-password" type="password" autoComplete="current-password"
                  value={password} onChange={(e) => setPassword(e.target.value)} required />
              </div>
              <CaptchaWidget key={captchaRound} onToken={setCaptchaToken} />
              <button type="submit" className="ky-auth-submit" disabled={loading}>
                <LogIn aria-hidden="true" />
                <span>{loading ? 'Signing in…' : 'Sign in'}</span>
              </button>
              {providers.length > 0 && <>
                <hr className="ky-auth-divider" />
                {providers.map((provider) => <a key={provider.id} href={provider.login_url} className="ky-auth-sso">
                  <Key aria-hidden="true" />
                  <span>Continue with {providers.length === 1 ? 'single sign-on' : provider.name}</span>
                </a>)}
              </>}
            </form>
          )}
        </div>
        <SetupGuide appURL={appURL} />
      </div>
    </main>
  );
};
