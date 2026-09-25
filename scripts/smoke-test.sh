#!/usr/bin/env bash
# End-to-end smoke test: runs the built binary and exercises the HTTP surface
# and the CLI subcommands. Usage: scripts/smoke-test.sh [path-to-binary]
set -euo pipefail

BIN="$(cd "$(dirname "$0")/.." && pwd)/${1:-kyyard-server}"
[ -x "$BIN" ] || BIN="${1:?binary not found; build with 'make build'}"

WORK="$(mktemp -d)"
PORT="${KY_SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
ADMIN_PASS="SmokeTestAdminPass123!"
SERVER_PID=""
FAILURES=0

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || :
    wait "$SERVER_PID" 2>/dev/null || :
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  [ok]   %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAILURES=$((FAILURES + 1)); }

check() { # check <description> <actual> <expected>
  if [ "$2" = "$3" ]; then pass "$1 ($2)"; else fail "$1: got '$2', want '$3'"; fi
}

contains() { # contains <description> <haystack> <needle>
  case "$2" in
  *"$3"*) pass "$1" ;;
  *) fail "$1: missing '$3'" ;;
  esac
}

status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

start_server() { # start_server <captcha-provider>
  # Automatic local Docker is covered by agent-install-test.py with isolated containers.
  # Keep this auth/manual-enrollment fixture independent of the runner's Docker access.
  KY_SESSION_SECRET='' \
    KY_ENCRYPTION_KEY='' \
    KY_DOCKER_SOCKET='' \
    KY_PORT="$PORT" \
    KY_HOST=127.0.0.1 \
    KY_DATA_DIR="$WORK/data" \
    KY_BACKUP_DIR="$WORK/backups" \
    KY_DB_DRIVER=sqlite \
    KY_ADMIN_PASSWORD="$ADMIN_PASS" \
    KY_CAPTCHA_PROVIDER="$1" \
    KY_SCIM_ENABLED=true \
    "$BIN" >"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  curl -s -o /dev/null --retry 30 --retry-delay 1 --retry-all-errors "$BASE/" ||
    { cat "$WORK/server.log"; fail "server did not come up"; exit 1; }
}

stop_server() {
  kill "$SERVER_PID"
  if wait "$SERVER_PID"; then pass "graceful shutdown on SIGTERM"; else fail "unclean shutdown"; fi
  SERVER_PID=""
}

echo "==> CLI subcommands"
check "version exits 0" "$("$BIN" version >/dev/null 2>&1 && echo 0 || echo 1)" "0"
contains "version prints name" "$("$BIN" version)" "kyyard-server"

# The drill seals to a throwaway key and reopens it, so the pipeline runs even unpaired.
# Whether the suite key is pinned is the status route's report, not the drill's.
DRILL_OUT="$(KY_DATA_DIR="$WORK/data" KY_PORT="$PORT" KY_DB_DRIVER=sqlite "$BIN" backup-drill)"
contains "backup-drill seals and reopens the payload" "$DRILL_OUT" "extracted into a 0700 sandbox"
contains "backup-drill verifies the required files" "$DRILL_OUT" "required files verified"
contains "backup-drill checks database integrity" "$DRILL_OUT" "integrity_check passed"
contains "backup-drill passes on a complete payload" "$DRILL_OUT" "Status:   PASSED"

check "init-admin rejects short password" \
  "$(KY_DATA_DIR="$WORK/cli" KY_DB_DRIVER=sqlite "$BIN" init-admin -password short >/dev/null 2>&1 && echo 0 || echo 1)" "1"
check "init-admin creates admin" \
  "$(KY_DATA_DIR="$WORK/cli" KY_DB_DRIVER=sqlite "$BIN" init-admin -password "$ADMIN_PASS" >/dev/null 2>&1 && echo 0 || echo 1)" "0"

echo "==> HTTP with default PoW captcha"
ADMIN_PASS=""
start_server pow
contains "liveness" "$(curl -sf "$BASE/health/live")" '"status":"ok"'
contains "readiness" "$(curl -sf "$BASE/health/ready")" '"status":"ok"'
if KY_HOST=127.0.0.1 KY_PORT="$PORT" "$BIN" healthcheck; then pass "binary readiness probe"; else fail "binary readiness probe"; fi
ADMIN_PASS="$(sed -n 's/.*Username: admin | Password: //p' "$WORK/server.log")"
check "fresh production boot prints one generated credential" "$(grep -c 'Initial bootstrap:' "$WORK/server.log")" "1"
check "generated bootstrap password is present" "$(test -n "$ADMIN_PASS" && echo yes || echo no)" "yes"
KEYS_BEFORE="$(sha256sum "$WORK/data/encryption.key" "$WORK/data/session.key" "$WORK/data/instance.key")"
check "GET / serves the PWA" "$(status "$BASE/")" "200"
contains "index.html has react root" "$(curl -s "$BASE/")" 'id="root"'
check "SPA fallback for unknown route" "$(status "$BASE/settings/deep/link")" "200"
check "login blocked without captcha token" \
  "$(status -X POST -H 'Content-Type: application/json' -d '{"username":"admin","password":"'"$ADMIN_PASS"'"}' "$BASE/api/auth/login")" "403"
check "malformed login body rejected" \
  "$(status -X POST -H 'Content-Type: application/json' -d 'not-json' "$BASE/api/auth/login")" "400"
check "login rejects GET" "$(status "$BASE/api/auth/login")" "405"
check "pow challenge issued" "$(status "$BASE/api/auth/pow-challenge")" "200"
contains "unauthenticated /me reports not authenticated" "$(curl -s "$BASE/api/auth/me")" '"authenticated":false' 
check "SCIM is retired" "$(status "$BASE/scim/v2/Users")" "404"
check "anonymous cannot export the capsule" "$(status -X POST "$BASE/api/backup/export-capsule")" "401"
check "anonymous cannot run backup drill" "$(status -X POST "$BASE/api/backup/drill")" "401"
check "anonymous cannot pair remote recovery" "$(status -X POST "$BASE/api/backup/pair-remote")" "401"
check "anonymous cannot read backup status" "$(status "$BASE/api/backup/status")" "401"
check "anonymous cannot pin a key" "$(status -X POST "$BASE/api/backup/pin-key")" "401"
check "anonymous cannot set the schedule" "$(status -X PUT "$BASE/api/backup/schedule")" "401"
check "anonymous cannot unpair" "$(status -X DELETE "$BASE/api/backup/pairing")" "401"
check "anonymous cannot set site theme" "$(status -X POST -H 'Content-Type: application/json' -d '{"theme":"oled"}' "$BASE/api/settings/theme")" "401"
check "SCIM stays retired with a bearer" "$(status -H 'Authorization: Bearer wrong' "$BASE/scim/v2/Users")" "404"
stop_server

echo "==> HTTP auth flow (captcha disabled)"
start_server none
check "restart does not print a bootstrap credential" "$(grep -c 'Initial bootstrap:' "$WORK/server.log" || true)" "0"
check "all durable keys survive restart" "$(sha256sum "$WORK/data/encryption.key" "$WORK/data/session.key" "$WORK/data/instance.key")" "$KEYS_BEFORE"
check "wrong password is 401" \
  "$(status -X POST -H 'Content-Type: application/json' -d '{"username":"admin","password":"wrong-password"}' "$BASE/api/auth/login")" "401"
check "unknown user is 401" \
  "$(status -X POST -H 'Content-Type: application/json' -d '{"username":"nobody","password":"'"$ADMIN_PASS"'"}' "$BASE/api/auth/login")" "401"

LOGIN_HEADERS="$WORK/login.headers"
LOGIN_BODY="$(curl -s -D "$LOGIN_HEADERS" -c "$WORK/cookies" -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"'"$ADMIN_PASS"'"}' "$BASE/api/auth/login")"
contains "login succeeds" "$LOGIN_BODY" '"authenticated":true'
contains "session cookie is HttpOnly" "$(grep -i '^set-cookie' "$LOGIN_HEADERS")" "HttpOnly"
contains "session cookie is SameSite" "$(grep -i '^set-cookie' "$LOGIN_HEADERS")" "SameSite"
contains "password hash never leaves the server" \
  "$(if echo "$LOGIN_BODY" | grep -q 'argon2'; then echo leaked; else echo clean; fi)" "clean"

contains "/me returns the session user" "$(curl -s -b "$WORK/cookies" "$BASE/api/auth/me")" '"username":"admin"'
ANON_SETTINGS="$(curl -s "$BASE/api/settings")"
contains "anonymous settings keep login fields" "$ANON_SETTINGS" '"app_name"'
contains "anonymous settings hide extra_settings" \
  "$(if echo "$ANON_SETTINGS" | grep -q 'extra_settings'; then echo leaked; else echo hidden; fi)" "hidden"
contains "anonymous settings hide db_driver" \
  "$(if echo "$ANON_SETTINGS" | grep -q 'db_driver'; then echo leaked; else echo hidden; fi)" "hidden"
contains "bootstrap requires password replacement" "$LOGIN_BODY" '"must_change_password":true'
check "bootstrap session cannot read backup state" "$(status -b "$WORK/cookies" "$BASE/api/backup/status")" "403"
CSRF="$(awk '$6 == "ky_csrf" { print $7 }' "$WORK/cookies")"
check "password replacement requires CSRF" "$(status -b "$WORK/cookies" -X POST "$BASE/api/auth/change-password")" "403"
check "bootstrap password replacement succeeds" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    -d '{"current_password":"'"$ADMIN_PASS"'","new_password":"ReplacementSmokePass456!"}' "$BASE/api/auth/change-password")" "200"
contains "bootstrap session was revoked" "$(curl -s -b "$WORK/cookies" "$BASE/api/auth/me")" '"authenticated":false'
check "old bootstrap password stops working" \
  "$(status -H 'Content-Type: application/json' -d '{"username":"admin","password":"'"$ADMIN_PASS"'"}' "$BASE/api/auth/login")" "401"
LOGIN_BODY="$(curl -s -c "$WORK/cookies" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"ReplacementSmokePass456!"}' "$BASE/api/auth/login")"
contains "replacement password signs in" "$LOGIN_BODY" '"authenticated":true'
contains "replacement clears the restriction" "$LOGIN_BODY" '"must_change_password":false'
check "init-admin resets the existing admin" \
  "$(KY_DATA_DIR="$WORK/data" KY_DB_DRIVER=sqlite "$BIN" init-admin -password 'OperatorResetPass789!' >/dev/null 2>&1 && echo 0 || echo 1)" "0"
check "operator reset revokes the previous session" "$(status -b "$WORK/cookies" "$BASE/api/backup/status")" "401"
LOGIN_BODY="$(curl -s -c "$WORK/cookies" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"OperatorResetPass789!"}' "$BASE/api/auth/login")"
contains "operator reset requires replacement" "$LOGIN_BODY" '"must_change_password":true'
check "reset login cannot read backup state" "$(status -b "$WORK/cookies" "$BASE/api/backup/status")" "403"
CSRF="$(awk '$6 == "ky_csrf" { print $7 }' "$WORK/cookies")"
check "operator password replacement succeeds" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    -d '{"current_password":"OperatorResetPass789!","new_password":"FinalSmokePassword123!"}' "$BASE/api/auth/change-password")" "200"
LOGIN_BODY="$(curl -s -c "$WORK/cookies" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"FinalSmokePassword123!"}' "$BASE/api/auth/login")"
contains "reset replacement signs in" "$LOGIN_BODY" '"authenticated":true'
contains "admin settings include db_driver" "$(curl -s -b "$WORK/cookies" "$BASE/api/settings")" '"db_driver"'
check "deposit CLI refuses without a key" \
  "$(KY_DATA_DIR="$WORK/cli" KY_DB_DRIVER=sqlite "$BIN" deposit >/dev/null 2>&1 && echo 0 || echo 1)" "1"
CSRF="$(awk '$6 == "ky_csrf" { print $7 }' "$WORK/cookies")"
# No key pinned, so the honest assertion is the documented refusal. 412 cannot come from the
# SPA fallback, which answers 200 for anything it does not recognise.
check "export-capsule is a POST behind CSRF" "$(status -b "$WORK/cookies" -X POST "$BASE/api/backup/export-capsule")" "403"
check "admin export-capsule refuses without a key" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/backup/export-capsule")" "412"
contains "export-capsule says why it refused" \
  "$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/backup/export-capsule")" "No recovery key"
STATUS_JSON="$(curl -s -b "$WORK/cookies" "$BASE/api/backup/status")"
contains "backup status reports no key" "$STATUS_JSON" '"key_pinned":false'
contains "backup status names the local directory" "$STATUS_JSON" "$WORK/backups"
check "backup status never carries a token" \
  "$(if printf '%s' "$STATUS_JSON" | grep -qi 'token'; then echo leaked; else echo clean; fi)" "clean"
check "pin-key refuses garbage" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"public_key":"AAAA","threshold":2,"total_shares":3}' -X POST "$BASE/api/backup/pin-key")" "400"
check "schedule refuses below the floor" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"interval_sec":60}' -X PUT "$BASE/api/backup/schedule")" "400"
check "schedule accepts off" \
  "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"interval_sec":0}' -X PUT "$BASE/api/backup/schedule")" "200"
contains "status reads the schedule back" "$(curl -s -b "$WORK/cookies" "$BASE/api/backup/status")" '"interval_sec":0'
check "run refuses without a key" "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/backup/deposit")" "412"
check "unpair refuses while unpaired" "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X DELETE "$BASE/api/backup/pairing")" "412"
check "cookie write rejects missing CSRF" "$(status -b "$WORK/cookies" -X POST "$BASE/api/devices/pair/init")" "403"
check "phone pairing is retired" "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/devices/pair/init")" "404"
check "phone pairing poll is retired" "$(status "$BASE/api/devices/pair/poll?secret=legacy")" "404"
stop_server
start_server none
contains "session survives an ordinary restart" "$(curl -s -b "$WORK/cookies" "$BASE/api/auth/me")" '"authenticated":true'
check "restart preserves the replaced admin" \
  "$(status -H 'Content-Type: application/json' -d '{"username":"admin","password":"FinalSmokePassword123!"}' "$BASE/api/auth/login")" "200"
# Agent lifecycle against the real binaries: enroll from stdin, pending, approve by the exact
# fingerprint, active through inventory, revocation stops the agent.
AGENT="$(dirname "$BIN")/kyyard-agent"
if [ -x "$AGENT" ]; then
  CSRF="$(awk '$6 == "ky_csrf" { print $7 }' "$WORK/cookies")"
  ENV_JSON="$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"name":"Smoke"}' "$BASE/api/organizations/org_initial/environments")"
  ENV_ID="$(printf '%s' "$ENV_JSON" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
  check "environment created for enrollment" "$(test -n "$ENV_ID" && echo yes || echo no)" "yes"
  TOKEN_JSON="$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"runtime":"docker"}' "$BASE/api/organizations/org_initial/environments/$ENV_ID/enrollment-tokens")"
  TOKEN="$(printf '%s' "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  contains "token response carries the socket disclosure" "$TOKEN_JSON" "root-equivalent"
  contains "HTTP-only installation explains remote setup" "$TOKEN_JSON" "Remote setup needs a reachable HTTPS address"
  printf '%s\n' "$TOKEN" | "$AGENT" --server "$BASE" --identity-dir "$WORK/agent" --name smoke-host >"$WORK/agent.log" 2>&1 &
  AGENT_PID=$!
  endpoint_state() { curl -s -b "$WORK/cookies" "$BASE/api/organizations/org_initial/endpoints" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p'; }
  wait_state() { for _ in $(seq 1 50); do [ "$(endpoint_state)" = "$1" ] && return 0; sleep 0.2; done; return 1; }
  wait_state pending || true
  check "agent enrolls as pending" "$(endpoint_state)" "pending"
  EP_JSON="$(curl -s -b "$WORK/cookies" "$BASE/api/organizations/org_initial/endpoints")"
  EP_ID="$(printf '%s' "$EP_JSON" | sed -n 's/.*"id":"\(ep_[^"]*\)".*/\1/p')"
  EP_FP="$(printf '%s' "$EP_JSON" | sed -n 's/.*"fingerprint":"\([0-9a-f]*\)".*/\1/p')"
  contains "agent prints the enrolled key fingerprint" "$(cat "$WORK/agent.log")" "agent key fingerprint: $EP_FP"
  check "identity file is owner-only" "$(stat -c '%a' "$WORK/agent/identity.json")" "600"
  check "identity file never holds the token" \
    "$(if grep -Fq -- "$TOKEN" "$WORK/agent/identity.json"; then echo leaked; else echo clean; fi)" "clean"
  SECOND_TOKEN_JSON="$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"runtime":"docker"}' "$BASE/api/organizations/org_initial/environments/$ENV_ID/enrollment-tokens")"
  SECOND_TOKEN="$(printf '%s' "$SECOND_TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  REENROLL_EXIT=0
  printf '%s\n' "$SECOND_TOKEN" | "$AGENT" --server "$BASE" --identity-dir "$WORK/agent" --enroll-only >"$WORK/reenroll.log" 2>&1 || REENROLL_EXIT=$?
  check "fresh enrollment refuses an existing identity" "$(test "$REENROLL_EXIT" -ne 0 && echo refused || echo accepted)" "refused"
  contains "refusal identifies the existing endpoint" "$(cat "$WORK/reenroll.log")" "$EP_ID"
  ENDPOINT_COUNT="$(curl -s -b "$WORK/cookies" "$BASE/api/organizations/org_initial/endpoints" | grep -o '"id":"ep_' | wc -l | tr -d ' ')"
  check "refused enrollment creates no second endpoint" "$ENDPOINT_COUNT" "1"
  KUBE_EXIT=0
  "$AGENT" --kubernetes --server "$BASE" >"$WORK/kube.log" 2>&1 || KUBE_EXIT=$?
  check "cluster agent refuses a Docker socket" "$(test "$KUBE_EXIT" -ne 0 && echo refused || echo accepted)" "refused"
  contains "refusal names the exclusive runtimes" "$(cat "$WORK/kube.log")" "kubernetes and docker are exclusive"
  check "cluster enrollment needs HTTPS" \
    "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"runtime":"kubernetes","name":"smoke-cluster"}' "$BASE/api/organizations/org_initial/environments/$ENV_ID/enrollment-tokens")" "409"
  check "approval binds the enrolled fingerprint" \
    "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{"fingerprint":"'"$EP_FP"'"}' -X POST "$BASE/api/organizations/org_initial/endpoints/$EP_ID/approve")" "204"
  wait_state active || true
  check "agent becomes active after approval" "$(endpoint_state)" "active"
  INV="$(curl -s -b "$WORK/cookies" "$BASE/api/organizations/org_initial/endpoints/$EP_ID/inventory")"
  contains "inventory is stored with its generation" "$INV" '"generation"'
  contains "inventory carries a snapshot" "$INV" '"containers"'
  check "inventory never carries container environment" \
    "$(if printf '%s' "$INV" | grep -qi '"env"'; then echo leaked; else echo clean; fi)" "clean"
  check "revoke closes the live agent" \
    "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/organizations/org_initial/endpoints/$EP_ID/revoke")" "204"
  AGENT_EXIT=0
  for _ in $(seq 1 50); do if ! kill -0 "$AGENT_PID" 2>/dev/null; then break; fi; sleep 0.2; done
  wait "$AGENT_PID" || AGENT_EXIT=$?
  check "agent exits on revocation" "$AGENT_EXIT" "2"
  contains "agent log names the revocation" "$(cat "$WORK/agent.log")" "revoked"
  contains "audit records the agent connection" "$(curl -s -b "$WORK/cookies" "$BASE/api/organizations/org_initial/audit")" '"action":"agent.connect"'
else
  echo "  [skip] kyyard-agent not built; agent lifecycle not exercised"
fi
check "logout succeeds" "$(status -b "$WORK/cookies" -c "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/auth/logout")" "200"
contains "session dead after logout" "$(curl -s -b "$WORK/cookies" "$BASE/api/auth/me")" '"authenticated":false' 
stop_server

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "smoke test: all checks passed"
else
  echo "smoke test: $FAILURES check(s) failed"
  exit 1
fi
