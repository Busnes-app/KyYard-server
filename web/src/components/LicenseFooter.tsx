import { useEffect, useRef, useState } from 'react';
import mitLicenseText from '../mit.txt?raw';

interface LicenseFooterProps {
  appName: string;
  // From the signed-in tier of /api/settings; empty hides the badge.
  version?: string;
}

// The suite's shared footer: copyright line that opens the About dialog with the MIT licence.
export function LicenseFooter({ appName, version }: LicenseFooterProps) {
  const [open, setOpen] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  const year = new Date().getFullYear();
  useEffect(() => {
    const d = dialog.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    else if (!open && d.open) d.close();
  }, [open]);
  return <div className="sidebar-footer">
    <p>
      <button type="button" className="license-link" onClick={() => setOpen(true)}>
        &copy; {year} &middot; MIT License
      </button>
    </p>
    <dialog
      ref={dialog}
      className="license-dialog"
      aria-label={`About ${appName}`}
      onClose={() => setOpen(false)}
      onClick={(event) => { if (event.target === dialog.current) setOpen(false); }}
    >
      <div className="license-window">
        <div className="license-window-header">
          <div className="license-window-title">
            <div className="license-title-main">
              <span className="license-app-name">{appName}</span>
              {version ? <span className="license-version-badge">v{version}</span> : null}
            </div>
            <p className="license-title-sub">Developed by Busnes.app</p>
            <p className="license-title-sub">&copy; {year} &middot; Licensed under the MIT License</p>
          </div>
          <button type="button" className="btn-secondary" onClick={() => setOpen(false)}>Close</button>
        </div>
        <textarea className="license-text" readOnly aria-label="MIT License" value={mitLicenseText} />
      </div>
    </dialog>
  </div>;
}
