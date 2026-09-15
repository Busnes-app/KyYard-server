import React, { useState } from 'react';
import { Shield, Key, AlertCircle, LogIn } from 'lucide-react';
import { CaptchaWidget } from '../components/CaptchaWidget';
import { ThemeSwitcher } from '../components/ThemeSwitcher';

interface LoginProps {
  onSuccess: (user: any) => void;
  appName: string;
}

interface MFAChallenge {
  mfa_token: string;
}

export const Login: React.FC<LoginProps> = ({ onSuccess, appName }) => {
  const [username, setUsername] = useState<string>('');
  const [password, setPassword] = useState<string>('');
  const [captchaToken, setCaptchaToken] = useState<string>('');
  const [mfaChallenge, setMfaChallenge] = useState<MFAChallenge | null>(null);
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
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '20px',
        background: 'var(--bg)',
      }}
    >
      <div style={{ position: 'absolute', top: 20, right: 20 }}>
        <ThemeSwitcher />
      </div>

      <div style={{ width: '100%', maxWidth: '420px' }}>
        <div style={{ textAlign: 'center', marginBottom: '24px' }}>
          <div
            style={{
              display: 'inline-flex',
              padding: '12px',
              background: 'var(--accent-soft)',
              borderRadius: '12px',
              color: 'var(--accent)',
              marginBottom: '12px',
            }}
          >
            <Shield size={32} />
          </div>
          <h1 style={{ fontSize: '24px', fontWeight: 'bold' }}>{appName || 'Busnes.app'}</h1>
          <p style={{ color: 'var(--ink)', fontSize: '14px', marginTop: '4px' }}>Cloud Mobile First Base Platform</p>
        </div>

        <div className="panel">
          {error && (
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: '8px',
                background: 'rgba(239, 68, 68, 0.1)',
                border: '1px solid var(--danger)',
                borderRadius: '6px',
                padding: '10px',
                color: 'var(--danger)',
                fontSize: '13px',
                marginBottom: '16px',
              }}
            >
              <AlertCircle size={16} />
              <span>{error}</span>
            </div>
          )}

          {mfaChallenge ? (
            <form onSubmit={handleMFASubmit}>
              <h2 style={{ fontSize: '18px', marginBottom: '8px' }}>Two-Factor Verification</h2>
              <p style={{ color: 'var(--ink)', fontSize: '13px', marginBottom: '16px' }}>
                {isRecoveryCode ? 'Enter one of your single-use recovery codes.' : 'Enter the 6-digit code from your authenticator app.'}
              </p>

              <div style={{ marginBottom: '16px' }}>
                <label style={{ display: 'block', fontSize: '13px', color: 'var(--ink)', marginBottom: '6px' }}>
                  {isRecoveryCode ? 'Recovery Code' : 'Authenticator Code'}
                </label>
                <input
                  type="text"
                  autoFocus
                  value={mfaCode}
                  onChange={(e) => setMfaCode(e.target.value)}
                  placeholder={isRecoveryCode ? 'XXXX-XXXX' : '123456'}
                  style={{ fontFamily: 'var(--font-mono)', fontSize: '18px', letterSpacing: '2px', textAlign: 'center' }}
                  required
                />
              </div>

              <button type="submit" style={{ width: '100%', justifyContent: 'center' }} disabled={loading}>
                {loading ? 'Verifying...' : 'Verify & Sign In'}
              </button>

              <div style={{ textAlign: 'center', marginTop: '16px' }}>
                <button
                  type="button"
                  className="btn-secondary"
                  style={{ background: 'transparent', border: 'none', color: 'var(--accent)', fontSize: '12px' }}
                  onClick={() => setIsRecoveryCode(!isRecoveryCode)}
                >
                  {isRecoveryCode ? 'Use Authenticator App' : 'Use a Recovery Code'}
                </button>
              </div>
            </form>
          ) : (
            <form onSubmit={handleLogin}>
              <div style={{ marginBottom: '16px' }}>
                <label style={{ display: 'block', fontSize: '13px', color: 'var(--ink)', marginBottom: '6px' }}>Username</label>
                <input
                  type="text"
                  autoFocus
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  placeholder="admin"
                  required
                />
              </div>

              <div style={{ marginBottom: '16px' }}>
                <label style={{ display: 'block', fontSize: '13px', color: 'var(--ink)', marginBottom: '6px' }}>Password</label>
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="••••••••••••"
                  required
                />
              </div>

              <CaptchaWidget onToken={(tok) => setCaptchaToken(tok)} />

              <button type="submit" style={{ width: '100%', justifyContent: 'center', marginTop: '8px' }} disabled={loading}>
                <LogIn size={16} />
                <span>{loading ? 'Signing in...' : 'Sign In'}</span>
              </button>

              <div style={{ marginTop: '20px', borderTop: '1px solid var(--line)', paddingTop: '16px' }}>
                <div style={{ fontSize: '12px', color: 'var(--ink)', textAlign: 'center', marginBottom: '12px' }}>
                  Or continue with Single Sign-On
                </div>
                <a
                  href="/api/sso/kysignon/login"
                  className="btn btn-secondary"
                  style={{ width: '100%', justifyContent: 'center', textDecoration: 'none' }}
                >
                  <Key size={16} style={{ color: 'var(--accent)' }} />
                  <span>KySignOn Identity</span>
                </a>
              </div>
            </form>
          )}
        </div>
      </div>
    </div>
  );
};
