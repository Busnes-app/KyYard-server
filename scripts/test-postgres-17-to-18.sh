#!/usr/bin/env bash
# Proves docker-compose.postgres.yml moves a real PostgreSQL 17 install to 18 on the next `up`:
# 17 data seeded with the previous overlay comes back with its rows, roles and passwords; the
# 17 volume still starts under the previous overlay; a dump beside a live 17 refuses; `up -d` over
# the running 17 stack moves it; a rerun is a no-op; a dump that cannot be restored never yields
# an empty cluster; a fresh install initialises. Needs Docker, git and
# openssl; starts only the postgres services, under throwaway compose projects.
# usage: scripts/test-postgres-17-to-18.sh
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
# The last commit whose overlay ran PostgreSQL 17 at /var/lib/postgresql/data.
old_rev=${KY_PG17_REV:-e0317dd8e270ff1041205f3f37fa121abd7f7b54}
work=$(mktemp -d)
proj=kypg1718-$$
fresh=$proj-fresh
P18=postgres:18.7-alpine3.24@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71

cleanup() {
  for p in "$proj" "$fresh"; do
    docker compose -p "$p" -f docker-compose.yml -f docker-compose.postgres.yml down -v -t 5 >/dev/null 2>&1 || true
  done
  docker volume rm -f "${proj}_pgdata" "${proj}_postgres" "${fresh}_pgdata" "${fresh}_postgres" >/dev/null 2>&1 || true
  docker run --rm -v "$work:/w" --entrypoint rm "$P18" -rf /w/tls >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; dc new logs --no-color --tail 30 >&2 || true; exit 1; }

git cat-file -e "$old_rev:docker-compose.postgres.yml" 2>/dev/null || git fetch -q --depth=1 origin "$old_rev"
git show "$old_rev:docker-compose.postgres.yml" > "$work/old.yml"
grep -q 'image: postgres:17-alpine@' "$work/old.yml" || fail "$old_rev does not hold the 17 overlay"

# The overlay needs server TLS; the postgres user (uid 70) must own the key.
mkdir "$work/tls"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=postgres \
  -keyout "$work/tls/server.key" -out "$work/tls/server.crt" 2>/dev/null
cp "$work/tls/server.crt" "$work/tls/ca.crt"
docker run --rm -v "$work/tls:/tls" --entrypoint sh "$P18" -c 'chown 70:70 /tls/server.key && chmod 600 /tls/server.key'
export KY_POSTGRES_PASSWORD=oldpass KY_POSTGRES_TLS_DIR=$work/tls \
  KY_DB_DSN='postgres://kyyard@postgres/kyyard?sslmode=verify-full&sslrootcert=/etc/kyyard-db-ca.crt'

# dc old|new|fresh <compose args>: the previous overlay, this one, this one on empty volumes.
dc() {
  local which=$1; shift
  case $which in
  old) docker compose -p "$proj" -f docker-compose.yml -f "$work/old.yml" "$@" ;;
  new) docker compose -p "$proj" -f docker-compose.yml -f docker-compose.postgres.yml "$@" ;;
  fresh) docker compose -p "$fresh" -f docker-compose.yml -f docker-compose.postgres.yml "$@" ;;
  esac
}
up() { dc "$1" up -d --wait --wait-timeout 300 postgres >/dev/null 2>&1; }
down() { dc "$1" down -t 30 >/dev/null 2>&1; }
# q <compose> <user> <password> <db> <sql>: from another container on the project network, so
# pg_hba's scram-sha-256 line applies (loopback inside the server container is trust).
q() {
  local p=$proj; [ "$1" = fresh ] && p=$fresh
  docker run --rm --network "${p}_default" -e PGPASSWORD="$3" "$P18" psql -h postgres -U "$2" -d "$4" -XAtc "$5"
}
in_vol() { docker run --rm -v "$1:/v" --entrypoint sh "$P18" -c "$2"; }

# 1. Real 17 data, written by the previous overlay.
up old || fail "the previous overlay did not start 17"
dc old exec -T postgres psql -U kyyard -d kyyard -X -q -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
create role reader login password 'readerpass';
create table t(id bigserial primary key, s text unique);
insert into t(s) select 'row' || g from generate_series(1, 20000) g;
create database cdb owner reader locale 'C' encoding 'UTF8' template template0;
\connect cdb reader
create table c(x text collate "C" primary key);
insert into c values ('a'), ('B'), ('é');
SQL

# 1b. A dump run by hand beside the live 17 refuses and writes nothing.
out=$(dc new run --rm postgres-17-dump 2>&1) && fail "the dump ran beside a live 17: $out"
grep -q postmaster.pid <<<"$out" || fail "no live-server message: $out"
in_vol "${proj}_postgres" 'test ! -e /v/upgrade-from-17' || fail "the dump wrote beside a live 17"
[ "$(q old kyyard oldpass kyyard 'select count(*) from t')" = 20000 ] || fail "17 hurt by the refused dump"

# 2. The next `up -d` with this overlay, over the running 17 stack (no `down`), moves it.
up new || fail "18 did not start after the move"
dc new logs --no-color postgres-17-dump | grep -q 'postgres-17-to-18: dumped' || fail "the dump did not run"
[ "$(q new kyyard oldpass kyyard 'show server_version_num' | cut -c1-2)" = 18 ] || fail "not 18"
[ "$(q new kyyard oldpass kyyard "select count(*) || ',' || max(s) from t")" = "20000,row9999" ] || fail "rows"
[ "$(q new reader readerpass cdb "select string_agg(x, ',' order by x) from c")" = "B,a,é" ] || fail "reader login or C collation"
q new kyyard wrongpass kyyard 'select 1' >/dev/null 2>&1 && fail "a wrong password logged in"
[ "$(q new kyyard oldpass kyyard 'show data_directory')" = /var/lib/postgresql/18/docker ] || fail "data directory"
[ "$(q new kyyard oldpass kyyard 'show ssl')" = on ] || fail "the overlay's TLS flags were lost"
in_vol "${proj}_pgdata" 'grep -qx 17 /v/PG_VERSION' || fail "the 17 volume changed"

# 3. Rerun: neither the dump nor the restore runs again.
down new; up new || fail "18 did not restart"
dc new logs --no-color postgres-17-dump | grep -q 'already done' || fail "rerun dumped again"
dc new logs --no-color postgres | grep -q 'postgres-17-to-18: restored' && fail "rerun restored again"
[ "$(q new kyyard oldpass kyyard 'select count(*) from t')" = 20000 ] || fail "rows after rerun"
down new

# 4. Rollback: the previous overlay still starts 17 on the kept volume.
up old || fail "17 no longer starts on its data"
[ "$(q old kyyard oldpass kyyard 'select count(*) from t')" = 20000 ] || fail "17 data changed"
down old

# 5. A dump that cannot be restored never yields a cluster, however often postgres restarts.
in_vol "${proj}_postgres" 'mv /v/18/docker /v/18/docker.saved && cp /v/upgrade-from-17/dump.sql /v/dump.good && echo "THIS IS NOT SQL;" >> /v/upgrade-from-17/dump.sql'
dc new up -d postgres >/dev/null 2>&1
id=$(dc new ps -aq postgres)
for _ in $(seq 1 180); do
  [ "$(docker inspect -f '{{.RestartCount}}' "$id")" -ge 2 ] && break
  read -rt 1 <> <(:) || true
done
[ "$(docker inspect -f '{{.RestartCount}}' "$id")" -ge 2 ] || fail "postgres did not retry the bad dump"
down new
in_vol "${proj}_postgres" 'test ! -e /v/18/docker' || fail "a bad dump produced a cluster"
in_vol "${proj}_postgres" 'mv /v/dump.good /v/upgrade-from-17/dump.sql && mv /v/18/docker.saved /v/18/docker'
up new || fail "18 did not start once the dump was repaired"
down new

# 6. A fresh install: nothing to move, a normal initdb.
up fresh || fail "a fresh install did not start"
dc fresh logs --no-color postgres-17-dump | grep -q 'nothing to move' || fail "a fresh install dumped something"
[ "$(q fresh kyyard oldpass kyyard 'show data_directory')" = /var/lib/postgresql/18/docker ] || fail "fresh data directory"
echo "PASS: 17 -> 18 through docker-compose.postgres.yml: live-17 dump refused, up over running 17, rerun, rollback, bad dump, fresh install"
