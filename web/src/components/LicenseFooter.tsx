import { useEffect, useRef, useState } from 'react';
import mitLicenseText from '../mit.txt?raw';

interface LicenseFooterProps {
  appName: string;
  // From the signed-in tier of /api/settings; empty hides the badge.
  version?: string;
  // The sidebar foot by default; `ky-content-foot` closes a phone page.
  className?: string;
}

// The suite's licence line and About dialog (ky-ui shell markup).
export function LicenseFooter({ appName, version, className = 'ky-license' }: LicenseFooterProps) {
  const [open, setOpen] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  const year = new Date().getFullYear();
  useEffect(() => {
    const d = dialog.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    else if (!open && d.open) d.close();
  }, [open]);
  return <div className={className}>
    <button type="button" className="ky-license-link" onClick={() => setOpen(true)}>
      &copy; {year} &middot; MIT License
    </button>
    <dialog
      ref={dialog}
      className="ky-about"
      aria-label={`About ${appName}`}
      onClose={() => setOpen(false)}
      onClick={(event) => { if (event.target === dialog.current) setOpen(false); }}
    >
      <div className="ky-about-head">
        <div>
          <h2 className="ky-about-name">{appName}{version ? <span className="ky-about-version">v{version}</span> : null}</h2>
          <p className="ky-about-meta">Developed by Busnes.app</p>
          <p className="ky-about-meta">&copy; {year} &middot; Licensed under the MIT License</p>
        </div>
        <button type="button" className="ky-about-close" onClick={() => setOpen(false)}>Close</button>
      </div>
      <textarea className="ky-about-license" readOnly aria-label="MIT License" value={mitLicenseText} />
    </dialog>
  </div>;
}
