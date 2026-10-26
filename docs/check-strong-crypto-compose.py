"""Compare complete synthetic Compose configurations; start no services."""

import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix="kyyard-profile-check-") as directory:
    fixture = Path(directory) / ".env"
    fixture.write_text(
        "KY_APP_URL=https://yard.example.invalid\n"
        "KY_TRUSTED_PROXIES=192.0.2.10/32\n"
        "KY_DNS=192.0.2.53\n"
        "KY_PROXY_IP=192.0.2.10/32\n"
        "KY_DB_DSN=postgres://synthetic:fixture@postgres/kyyard?sslmode=verify-full\n"
        "KY_POSTGRES_PASSWORD=synthetic-fixture-password\n"
        "KY_POSTGRES_TLS_DIR=/synthetic/tls\n"
    )
    # An operator-supplied later env_file must retain precedence too.
    server_env = Path(directory) / "server.env"
    server_env.write_text("KY_ADMIN_PASSWORD=synthetic-operator-password\nKY_OIDC_ISSUER=https://identity.example.invalid\n")
    operator = Path(directory) / "operator.json"
    operator.write_text(json.dumps({"services": {"kyyard": {"env_file": [str(server_env)]}}}))
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("COMPOSE_", "KY_"))}

    def render(files):
        command = ["docker", "compose", "--project-name", "kyyard-profile-check",
                   "--env-file", str(fixture)]
        for file in files:
            command += ["-f", str(file)]
        return json.loads(subprocess.check_output(command + ["config", "--format", "json"],
                                                 cwd=root, env=env))

    chains = [[], ["docker-compose.build.yml"], ["docker-compose.bind.yml"],
              ["docker-compose.proxy.yml", "docker-compose.proxy-network.yml"],
              ["docker-compose.build.yml", "docker-compose.bind.yml", "docker-compose.lan-dns.yml"],
              ["docker-compose.postgres.yml", "docker-compose.proxy.yml"],
              ["docker-compose.proxy-container.yml"]]
    for overlays in chains:
        files = ["docker-compose.yml", *overlays, operator]
        baseline = render(files)
        configured = render([*files, "docker-compose.strong-crypto.yml"])
        settings = configured["services"]["kyyard"]["environment"]
        assert settings.pop("GODEBUG") == "fips140=on"
        assert settings["KY_ADMIN_PASSWORD"] == "synthetic-operator-password"
        assert configured == baseline, "The profile changed another Compose setting"
print("All seven chains retain operator env_files, credentials, data, image/build, network and DNS settings.")
