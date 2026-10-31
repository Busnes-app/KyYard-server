import React, { useEffect, useState } from 'react';
import { ShieldCheck, Loader2, AlertCircle } from 'lucide-react';

interface CaptchaProps {
  onToken: (token: string) => void;
}

interface Challenge {
  algorithm: string;
  salt: string;
  challenge: string;
  maxnumber: number;
  expires_at: number;
  signature: string;
}

export const CaptchaWidget: React.FC<CaptchaProps> = ({ onToken }) => {
  const [status, setStatus] = useState<'working' | 'done' | 'failed'>('working');
  const [progress, setProgress] = useState<number>(0);
  const [errorMsg, setErrorMsg] = useState<string>('');

  useEffect(() => {
    let cancelled = false;

    const runPoW = async () => {
      try {
        const resp = await fetch('/api/auth/pow-challenge');
        if (!resp.ok) throw new Error('Failed to fetch security challenge');
        const chal: Challenge = await resp.json();

        const enc = new TextEncoder();
        const max = chal.maxnumber;
        // Compare raw bytes: hex-encoding every candidate triples the solve time.
        const want = Uint8Array.from(chal.challenge.match(/../g) ?? [], (h) => parseInt(h, 16));
        let solved = false;

        for (let i = 1; i <= max; i++) {
          if (cancelled) return;
          const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', enc.encode(`${chal.salt}${i}`)));

          if (hash.length === want.length && hash.every((b, j) => b === want[j])) {
            const sol = {
              algorithm: chal.algorithm,
              salt: chal.salt,
              challenge: chal.challenge,
              number: i,
              maxnumber: chal.maxnumber,
              expires_at: chal.expires_at,
              signature: chal.signature,
            };
            const token = btoa(JSON.stringify(sol));
            setStatus('done');
            onToken(token);
            solved = true;
            break;
          }

          if (i % 2000 === 0) {
            setProgress(Math.round((i / max) * 100));
            await new Promise((r) => setTimeout(r, 0)); // yield loop
          }
        }

        if (!solved && !cancelled) {
          throw new Error('Could not solve challenge');
        }
      } catch (err) {
        if (!cancelled) {
          setStatus('failed');
          setErrorMsg(err instanceof Error ? err.message : 'Security check failed');
        }
      }
    };

    runPoW();
    return () => {
      cancelled = true;
    };
  }, [onToken]);

  return (
    <p className="ky-auth-check" data-ky-state={status === 'done' ? 'passed' : status === 'failed' ? 'failed' : undefined} aria-live="polite">
      {status === 'working' && <><Loader2 className="animate-spin" aria-hidden="true" /><span>Verifying security check ({progress}%)...</span></>}
      {status === 'done' && <><ShieldCheck aria-hidden="true" /><span>Browser check passed</span></>}
      {status === 'failed' && <><AlertCircle aria-hidden="true" /><span>{errorMsg}</span></>}
    </p>
  );
};
