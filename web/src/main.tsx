import './theme';
import React from 'react';
import ReactDOM from 'react-dom/client';
import { App } from './App';
import { applyTheme, getStoredTheme } from './theme';
applyTheme(getStoredTheme(), false);

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);

// External module execution complies with the server's script-src self policy.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => { void navigator.serviceWorker.register('/sw.js').catch(() => {}); });
}
