#!/usr/bin/env python3
"""Exercise automatic local Docker and one-command HTTPS enrollment against a built image.

Usage: python3 scripts/agent-install-test.py kyyard:ci
Creates uniquely named containers/volumes and removes only those fixtures.
"""
import http.cookiejar
import ssl
import tempfile
from pathlib import Path
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

image = sys.argv[1]
prefix = "ky-install-" + uuid.uuid4().hex[:10]
server, agent = prefix + "-server", prefix + "-agent"
data, identity = prefix + "-data", prefix + "-identity"
cookies = http.cookiejar.CookieJar()
# A separate proxy container models HTTPS between hosts on the default bridge.
proxy_name = prefix + "-proxy"
certs = tempfile.TemporaryDirectory(prefix="ky-link-tls-")
cert = str(Path(certs.name) / "cert.pem")
key = str(Path(certs.name) / "key.pem")
subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=yard.test",
                "-addext", "subjectAltName=DNS:yard.test,IP:127.0.0.1", "-keyout", key, "-out", cert], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
http = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=cert)), urllib.request.HTTPCookieProcessor(cookies))


def proxy_config(address):
    Path(certs.name, "default.conf").write_text("""server {
      listen 443 ssl;
      ssl_certificate /fixtures/cert.pem;
      ssl_certificate_key /fixtures/key.pem;
      location / {
        proxy_pass http://""" + address + """:9273;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
      }
    }
    """)


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True, stderr=subprocess.STDOUT).strip()


def wait_for(fn, description):
    for _ in range(60):
        try:
            value = fn()
            if value:
                return value
        except (urllib.error.URLError, ConnectionError, TypeError):
            pass
        time.sleep(0.5)
    raise RuntimeError("Timed out: " + description)


def api(path, body=None):
    headers = {}
    if body is not None:
        headers["Content-Type"] = "application/json"
        headers["X-CSRF-Token"] = next((c.value for c in cookies if c.name == "ky_csrf"), "")
    req = urllib.request.Request(base + path, headers=headers,
                                 data=None if body is None else json.dumps(body).encode())
    with http.open(req, timeout=5) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def start_server():
    server_id = docker("run", "-d", "--name", server, "--network", "bridge",
                       "-v", data + ":/data",
                       "-v", "/var/run/docker.sock:/var/run/docker.sock",
                       "-e", "KY_ALLOW_PLAINTEXT_BIND=true", "-e", "KY_CAPTCHA_PROVIDER=none",
                       "-e", "KY_APP_URL=https://yard.test", "-e", "KY_TRUSTED_PROXIES=" + proxy_ip, image)
    address = docker("inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', server)
    proxy_config(address)
    docker("exec", proxy_name, "nginx", "-s", "reload")
    wait_for(lambda: api("/health/ready"), "server readiness")
    return server_id


try:
    proxy_config("127.0.0.1")
    docker("run", "-d", "--name", proxy_name, "--network", "bridge", "-p", "127.0.0.1::443",
           "-v", certs.name + ":/fixtures:ro", "-v", str(Path(certs.name, "default.conf")) + ":/etc/nginx/conf.d/default.conf:ro", "nginx:alpine")
    proxy_ip = docker("inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', proxy_name)
    base = "https://127.0.0.1:" + docker("port", proxy_name, "443/tcp").rsplit(":", 1)[1]
    server_id = start_server()
    password = re.search(r"Username: admin \| Password: (\S+)", docker("logs", server)).group(1)
    api("/api/auth/login", {"username": "admin", "password": password})
    replacement = "InstallTestNewPassword123!"
    api("/api/auth/change-password", {"current_password": password, "new_password": replacement})
    api("/api/auth/login", {"username": "admin", "password": replacement})
    org = "/api/organizations/org_initial"
    local_ep = org + "/endpoints/ep_local_docker"

    def local_inventory():
        inv = api(local_ep + "/inventory")
        return inv if server_id in {c["id"] for c in inv["snapshot"]["containers"]} else None

    first_local = wait_for(local_inventory, "automatic local container inventory without enrollment")
    assert len(api(org + "/endpoints")) == 1
    assert api(local_ep)["state"] == "active"
    docker("restart", server)
    wait_for(lambda: api("/health/ready"), "readiness after restart")
    wait_for(lambda: local_inventory()["generation"] > first_local["generation"], "automatic local reconnect after restart")
    env = api(org + "/environments", {"name": "Installation test"})["id"]
    minted = api(org + "/environments/" + env + "/enrollment-tokens", {"runtime": "docker"})
    # Keep the generated run/link path; use the locally built image and test CA.
    command = minted["command"].removeprefix("sudo ").replace("--pull always", "--pull never")
    command = command.replace("'" + minted["image"] + "'", "'" + image + "'")
    command = command.replace("--name kyyard-agent ", "--name " + agent + " ")
    command = command.replace("-v kyyard-agent-identity:", "-v " + identity + ":")
    command = command.replace("--no-healthcheck", "--no-healthcheck --add-host yard.test:" + proxy_ip + " -e SSL_CERT_FILE=/test-ca.pem -v " + cert + ":/test-ca.pem:ro")
    result = subprocess.run(["sh", "-c", command], capture_output=True, text=True, timeout=30)
    if result.returncode:
        # Never print the command, because it carries the enrollment token.
        raise RuntimeError("Generated command failed: " + result.stderr)
    assert minted["token"] not in result.stdout, "Token leaked during enrollment"
    endpoint = wait_for(lambda: next((e for e in api(org + "/endpoints") if e["environment_id"] == env), None), "pending enrollment")
    assert endpoint["state"] == "pending", endpoint["state"]
    fingerprint = wait_for(lambda: re.search(r"agent key fingerprint: ([0-9a-f]{64})", docker("logs", agent)), "host fingerprint").group(1)
    assert endpoint["fingerprint"] == fingerprint, "Approval fingerprint mismatch"
    assert minted["token"] not in docker("logs", agent), "Token leaked in agent log"
    ep = org + "/endpoints/" + endpoint["id"]
    api(ep + "/approve", {"fingerprint": fingerprint})

    def inventory_has_fixtures():
        inventory = api(ep + "/inventory")
        ids = {c["id"] for c in inventory["snapshot"]["containers"]}
        return inventory if {server_id, docker("inspect", "--format", "{{.Id}}", agent)} <= ids else None

    inventory = wait_for(inventory_has_fixtures, "both fixture containers in inventory")
    assert docker("inspect", "--format", "{{.Image}}", agent) == docker("inspect", "--format", "{{.Image}}", server)
    assert docker("inspect", "--format", "{{.HostConfig.NetworkMode}}", server) == "bridge"
    docker("restart", agent)
    wait_for(lambda: api(ep + "/inventory")["received_at"] != inventory["received_at"], "fresh inventory after agent restart")
    assert len(api(org + "/endpoints")) == 2, "Restart duplicated an endpoint"
    # Remote agents reconnect after server replacement without recreation.
    assert docker("inspect", "--format", "{{.HostConfig.NetworkMode}}", agent) in ("default", "bridge")
    docker("stop", server)
    docker("rm", "-v", server)
    server_id = start_server()
    wait_for(local_inventory, "automatic local reconnect after server replacement")
    wait_for(inventory_has_fixtures, "inventory after server replacement")
    assert len(api(org + "/endpoints")) == 2, "Replacement duplicated an endpoint"
    assert "agent key fingerprint: " + fingerprint in docker("logs", agent)
    api(local_ep + "/revoke", {})
    docker("restart", server)
    wait_for(lambda: api("/health/ready"), "readiness after revoked local restart")
    assert api(local_ep)["state"] == "revoked", "Restart restored revoked local authority"
    print("PASS: automatic local inventory/restart/replacement/revocation and single-command HTTPS agent enrollment/approval/reconnect")
finally:
    for name in (agent, server, proxy_name):
        subprocess.run(["docker", "rm", "-fv", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for name in (identity, data):
        subprocess.run(["docker", "volume", "rm", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    certs.cleanup()
