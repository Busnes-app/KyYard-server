#!/usr/bin/env python3
"""Render both proxy profiles with Docker Compose; starts no containers."""
import json
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
env = dict(os.environ, KY_APP_URL='https://yard.example.test', KY_PROXY_IP='10.89.0.10',
           KYPOST_NETWORK='fixture-suite', KY_POSTGRES_TLS_DIR='/private/fixture-tls',
           KY_POSTGRES_PASSWORD='disposable-config-only-password',
           KY_DB_DSN='postgres://kyyard@postgres/kyyard?sslmode=verify-full&sslrootcert=/etc/kyyard-db-ca.crt')
for proxy_ip, postgres in ((ip, pg) for ip in ('10.89.0.10', 'fd00::20') for pg in (False, True)):
    env['KY_PROXY_IP'] = proxy_ip
    command = ['docker', 'compose', '-f', 'docker-compose.yml']
    if postgres:
        command += ['-f', 'docker-compose.postgres.yml']
    command += ['-f', 'docker-compose.proxy-container.yml', 'config', '--format', 'json']
    config = json.loads(subprocess.check_output(command, cwd=root, env=env))
    app = config['services']['kyyard']
    assert 'network_mode' not in app and not app.get('ports')
    assert app['environment']['KY_TRUSTED_PROXIES'] == proxy_ip
    assert app['environment']['KY_APP_URL'] == 'https://yard.example.test'
    assert set(app['networks']) == {'default', 'suite'}
    assert config['networks']['suite']['external'] is True
    assert config['networks']['suite']['name'] == 'fixture-suite'
    if postgres:
        database = config['services']['postgres']
        assert 'network_mode' not in database and set(database['networks']) == {'default'}
        assert 'sslmode=verify-full' in app['environment']['KY_DB_DSN']
print('PASS: fixed-address container proxy, no host publish, external ownership, PostgreSQL network and TLS')
