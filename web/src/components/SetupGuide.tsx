export function SetupGuide({ appURL }: { appURL: string }) {
  if (!appURL.startsWith('http://')) return null;
  return (
    <details className="panel" style={{ marginTop: 16, fontSize: 14 }}>
      <summary style={{ cursor: 'pointer' }}>Local setup and remote access</summary>
      <p>Open <a href={appURL}>{appURL}</a> on the Docker host. First sign-in: use
        <code> admin</code> and the temporary password in <code>docker compose logs kyyard</code>,
        then replace it.</p>
      <p>From another machine, use an SSH tunnel to localhost:8080, or configure an HTTPS
        reverse proxy with a valid certificate. Set <code>KY_APP_URL</code> to its HTTPS
        address and <code>KY_TRUSTED_PROXIES</code> to the proxy’s address, then enable
        <code> docker-compose.proxy.yml</code> in <code>COMPOSE_FILE</code> and restart.</p>
      <p>Keep port 8080 private. Do not publish this HTTP endpoint on your network.
        Agent enrollment is not available yet; remote enrollment will require HTTPS.</p>
    </details>
  );
}
