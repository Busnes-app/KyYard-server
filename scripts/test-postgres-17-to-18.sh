#!/usr/bin/env bash
# Proves docker-compose.postgres.yml moves a real PostgreSQL 17 install to 18 on the next `up`:
# 17 data seeded with the previous overlay comes back with its rows, roles and passwords; the
# 17 volume still starts under the previous overlay; a dump beside a live 17 refuses; `up -d` over
# the running 17 stack moves it; a rerun is a no-op; after a rollback that ran 17, 18 is refused
# until the operator moves 17 again or keeps 18, also after the documented clean-up; a dump that
# cannot be restored never yields an empty cluster; a fresh install initialises. Needs Docker, git and
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

# 4. Rollback: the previous overlay still starts 17 on the kept volume, and takes a write.
up old || fail "17 no longer starts on its data"
[ "$(q old kyyard oldpass kyyard 'select count(*) from t')" = 20000 ] || fail "17 data changed"
q old kyyard oldpass kyyard "insert into t(s) values ('written on 17 after the move')" >/dev/null
down old

# 4b. Back to this overlay after that rollback: 18 lacks the new row, so the stack refuses to
# start it and the dump says why and what the two choices are.
up new && fail "18 started although 17 ran after the move"
out=$(dc new logs --no-color postgres-17-dump)
grep -q 'has run on' <<<"$out" || fail "no rollback message: $out"
grep -q 'To move 17 again' <<<"$out" || fail "the message lacks the move-again choice: $out"
grep -q 'To keep 18' <<<"$out" || fail "the message lacks the keep-18 choice: $out"
[ -z "$(dc new ps -q postgres)" ] || fail "postgres runs after the refusal"
down new

# 4c. "Move 17 again": rename 18/ and the dump with a suffix; the new row reaches 18 and the
# replaced cluster is kept.
# The commands docs/RUNNING.md gives.
dc new run --rm --no-deps --entrypoint sh postgres -c 'cd /var/lib/postgresql && mv 18 18.replaced && { [ ! -e upgrade-from-17 ] || mv upgrade-from-17 upgrade-from-17.replaced; }' >/dev/null 2>&1 || fail "the documented move-again rename failed"
up new || fail "18 did not start after moving 17 again"
[ "$(q new kyyard oldpass kyyard 'select count(*) from t')" = 20001 ] || fail "moving again lost the row written on 17"
in_vol "${proj}_postgres" 'test -s /v/18.replaced/docker/PG_VERSION' || fail "the replaced 18 cluster is gone"
down new

# 4d. "Keep 18": after another rollback start, deleting the checkpoint is accepted.
up old || fail "17 did not start for the keep-18 check"
down old
up new && fail "18 started although 17 ran after the move (keep-18 check)"
down new
dc new run --rm --no-deps --entrypoint rm postgres /var/lib/postgresql/18/moved-from-17.checkpoint >/dev/null 2>&1 || fail "the documented keep-18 delete failed"
up new || fail "keep 18 was not accepted"
dc new logs --no-color postgres-17-dump | grep -q 'already done' || fail "keep 18 dumped again"
down new

# 4e. The documented clean-up (the dump removed, 17 kept): later starts go on, and a rollback
# that writes on 17 after it is still refused on the way back.
in_vol "${proj}_postgres" 'mv /v/upgrade-from-17 /v/upgrade-from-17.kept'
up new || fail "the stack did not start after the documented clean-up"
down new
up old || fail "17 did not start for the clean-up check"
q old kyyard oldpass kyyard "insert into t(s) values ('written on 17 after the clean-up')" >/dev/null
down old
up new && fail "after the clean-up, 18 started although 17 ran after the move"
dc new logs --no-color postgres-17-dump | grep -q 'has run on' || fail "no rollback message after the clean-up"
down new
# Keep 18 again and put the dump back for step 5.
in_vol "${proj}_postgres" 'rm /v/18/moved-from-17.checkpoint && mv /v/upgrade-from-17.kept /v/upgrade-from-17'
up new || fail "keep 18 after the clean-up check"
down new

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
echo "PASS: 17 -> 18 through docker-compose.postgres.yml: live-17 dump refused, up over running 17, rerun, rollback refused, move again, keep 18, clean-up, bad dump, fresh install"
