#!/usr/bin/env bash
# End-to-end smoke test: runs the built binary and exercises the HTTP surface
# and the CLI subcommands. Usage: scripts/smoke-test.sh [path-to-binary]
set -euo pipefail

BIN="$(cd "$(dirname "$0")/.." && pwd)/${1:-ky_server_base}"
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
contains "version prints name" "$("$BIN" version)" "ky_server_base"

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
start_server pow
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
check "scim rejects missing bearer" "$(status "$BASE/scim/v2/Users")" "401"
check "anonymous cannot export the capsule" "$(status -X POST "$BASE/api/backup/export-capsule")" "401"
check "anonymous cannot run backup drill" "$(status -X POST "$BASE/api/backup/drill")" "401"
check "anonymous cannot pair remote recovery" "$(status -X POST "$BASE/api/backup/pair-remote")" "401"
check "anonymous cannot read backup status" "$(status "$BASE/api/backup/status")" "401"
check "anonymous cannot pin a key" "$(status -X POST "$BASE/api/backup/pin-key")" "401"
check "anonymous cannot set the schedule" "$(status -X PUT "$BASE/api/backup/schedule")" "401"
check "anonymous cannot unpair" "$(status -X DELETE "$BASE/api/backup/pairing")" "401"
check "anonymous cannot set site theme" "$(status -X POST -H 'Content-Type: application/json' -d '{"theme":"oled"}' "$BASE/api/settings/theme")" "401"
check "scim rejects wrong bearer" "$(status -H 'Authorization: Bearer wrong' "$BASE/scim/v2/Users")" "401"
stop_server

echo "==> HTTP auth flow (captcha disabled)"
start_server none
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
check "device pairing init" "$(status -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/devices/pair/init")" "200"
# pair/poll is unauthenticated: holding the secret must not hand over the code, the user or
# the push token. Poll with a real secret and assert the projection.
PAIR_INIT="$(curl -s -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -X POST "$BASE/api/devices/pair/init")"
PAIR_SECRET="$(printf '%s' "$PAIR_INIT" | sed -n 's/.*"secret":"\([^"]*\)".*/\1/p')"
PAIR_POLL="$(curl -s "$BASE/api/devices/pair/poll?secret=$PAIR_SECRET")"
contains "pairing poll reports status" "$PAIR_POLL" '"status"'
check "pairing poll hides the secret" \
  "$(if printf '%s' "$PAIR_POLL" | grep -q '"secret"'; then echo leaked; else echo hidden; fi)" "hidden"
check "pairing poll hides the code" \
  "$(if printf '%s' "$PAIR_POLL" | grep -q '"code"'; then echo leaked; else echo hidden; fi)" "hidden"
check "pairing poll hides the push token" \
  "$(if printf '%s' "$PAIR_POLL" | grep -q '"push_token"'; then echo leaked; else echo hidden; fi)" "hidden"
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
