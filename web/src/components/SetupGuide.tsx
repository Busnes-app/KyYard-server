export function SetupGuide({ appURL }: { appURL: string }) {
  if (!appURL.startsWith('http://')) return null;
  // The port is read from the origin the operator is looking at rather than written here:
  // advice that names a port nothing listens on tells someone to protect the wrong thing.
  const port = portOf(appURL);
  return (
    <details className="panel" style={{ marginTop: 16, fontSize: 14 }}>
      <summary style={{ cursor: 'pointer' }}>Local setup and remote access</summary>
      <p>Open <a href={appURL}>{appURL}</a> on the Docker host. First sign-in: use
        <code> admin</code> and the temporary password in <code>docker compose logs kyyard</code>,
        then replace it.</p>
      <p>From another machine, use an SSH tunnel to localhost:{port}, or configure an HTTPS
        reverse proxy with a valid certificate. Set <code>KY_APP_URL</code> to its HTTPS
        address and <code>KY_TRUSTED_PROXIES</code> to the proxy’s address, then enable
        <code> docker-compose.proxy.yml</code> in <code>COMPOSE_FILE</code> and restart.</p>
      <p>Keep port {port} private. Do not publish this HTTP endpoint on your network.
        Agent enrollment is not available yet; remote enrollment will require HTTPS.</p>
    </details>
  );
}

// portOf answers with the port of an http origin, which is 80 when the URL names none.
function portOf(appURL: string): string {
  try {
    return new URL(appURL).port || '80';
  } catch {
    return '80';
  }
}
