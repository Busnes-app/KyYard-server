import React, { useEffect, useRef, useState } from 'react';
import QRCode from 'qrcode';
import { Smartphone, CheckCircle, X, Copy, Check } from 'lucide-react';
import { secureFetch } from '../api';

interface QRPairingModalProps {
  onClose: () => void;
}

export const QRPairingModal: React.FC<QRPairingModalProps> = ({ onClose }) => {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [code, setCode] = useState<string>('');
  const [secondsLeft, setSecondsLeft] = useState<number>(90);
  const [paired, setPaired] = useState<boolean>(false);
  const [deviceName, setDeviceName] = useState<string>('');
  const [copied, setCopied] = useState<boolean>(false);

  useEffect(() => {
    let timer: any;
    let pollInterval: any;

    const init = async () => {
      try {
        const resp = await secureFetch('/api/devices/pair/init', { method: 'POST' });
        const data = await resp.json();
        setCode(data.code);

        if (canvasRef.current && data.qr_payload) {
          QRCode.toCanvas(canvasRef.current, data.qr_payload, {
            width: 200,
            margin: 1,
            color: { dark: '#4deeea', light: '#0d0f14' },
          });
        }

        // Start 90s timer
        const expiry = Date.now() + 90 * 1000;
        timer = setInterval(() => {
          const remaining = Math.max(0, Math.round((expiry - Date.now()) / 1000));
          setSecondsLeft(remaining);
          if (remaining <= 0) clearInterval(timer);
        }, 1000);

        // Start polling
        pollInterval = setInterval(async () => {
          if (!data.secret) return;
          try {
            const pollResp = await fetch(`/api/devices/pair/poll?secret=${data.secret}`);
            if (pollResp.ok) {
              const p = await pollResp.json();
              if (p.status === 'approved') {
                setPaired(true);
                setDeviceName(p.device_name || 'Mobile Device');
                clearInterval(pollInterval);
                clearInterval(timer);
              }
            }
          } catch {}
        }, 1500);
      } catch (err) {
        console.error('Pairing init error:', err);
      }
    };

    init();

    return () => {
      clearInterval(timer);
      clearInterval(pollInterval);
    };
  }, []);

  const copyCode = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal-window" onClick={(e) => e.stopPropagation()}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '16px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
            <Smartphone size={20} style={{ color: 'var(--accent)' }} />
            <h3 style={{ fontSize: '18px' }}>Link Mobile Device</h3>
          </div>
          <button className="btn-secondary" style={{ padding: '4px' }} onClick={onClose} aria-label="Close modal">
            <X size={18} />
          </button>
        </div>

        {paired ? (
          <div style={{ textAlign: 'center', padding: '24px 0' }}>
            <CheckCircle size={48} style={{ color: 'var(--success)', margin: '0 auto 12px' }} />
            <h4 style={{ fontSize: '16px', color: 'var(--ink-strong)', marginBottom: '4px' }}>Device Linked!</h4>
            <p style={{ color: 'var(--ink)', fontSize: '14px' }}>{deviceName} is now securely paired with your account.</p>
            <button style={{ marginTop: '16px' }} onClick={onClose}>
              Done
            </button>
          </div>
        ) : (
          <div>
            <p style={{ color: 'var(--ink)', fontSize: '13px', marginBottom: '16px' }}>
              Scan this QR code in your KySecurity / Business.app mobile wrapper or enter the 6-digit PIN below.
            </p>

            <div
              style={{
                display: 'flex',
                justifyContent: 'center',
                background: '#0d0f14',
                padding: '16px',
                borderRadius: '8px',
                border: '1px solid var(--line)',
                marginBottom: '16px',
              }}
            >
              <canvas ref={canvasRef} />
            </div>

            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '16px' }}>
              <div>
                <div style={{ fontSize: '12px', color: 'var(--ink)' }}>Pairing PIN</div>
                <div style={{ fontSize: '24px', fontWeight: 'bold', fontFamily: 'var(--font-mono)', letterSpacing: '2px', color: 'var(--accent)' }}>
                  {code || '••••••'}
                </div>
              </div>
              <button type="button" className="btn-secondary" onClick={copyCode}>
                {copied ? <Check size={16} /> : <Copy size={16} />}
                {copied ? 'Copied' : 'Copy PIN'}
              </button>
            </div>

            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', fontSize: '12px', color: 'var(--ink)' }}>
              <span>Code expires in:</span>
              <span className="badge badge-accent" style={{ fontFamily: 'var(--font-mono)' }}>
                {secondsLeft}s
              </span>
            </div>
          </div>
        )}
      </div>
    </div>
  );
};
