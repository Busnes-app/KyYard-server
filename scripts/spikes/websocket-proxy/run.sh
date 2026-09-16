#!/usr/bin/env bash
# Re-runnable spike: builds the echo server and client, starts Caddy and nginx on the host
# network, runs the idle/heartbeat, proxy-restart and 4 MiB scenarios, prints RESULT lines.
set -euo pipefail
cd "$(dirname "$0")"
PROJECT=kyyard-ws-spike
cleanup() {
  docker compose -p "$PROJECT" down --remove-orphans >/dev/null 2>&1 || true
  [[ -n ${SERVER_PID:-} ]] && kill "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT
go build -o spike . 
./spike -mode server >server.log 2>&1 &
SERVER_PID=$!
docker compose -p "$PROJECT" up -d --quiet-pull
sleep 2
# Both proxies at once; nginx gets a restart 45 s into its idle phase.
./spike -mode client -name caddy -url ws://127.0.0.1:8081/ws >caddy.log 2>&1 &
C1=$!
./spike -mode client -name nginx -url ws://127.0.0.1:8082/ws -restart-after 45s >nginx.log 2>&1 &
C2=$!
sleep 45
docker compose -p "$PROJECT" restart nginx >/dev/null
wait $C1 || true
wait $C2 || true
cat caddy.log nginx.log
grep -q "RESULT caddy PASS" caddy.log && grep -q "RESULT nginx PASS" nginx.log && grep -q "reconnected" nginx.log
