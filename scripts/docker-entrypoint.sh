#!/bin/sh
# Image entrypoint. Started as root, it gives the kyyard user (uid 10001) the data directories
# and the mounted Docker socket's group, then runs the server as kyyard. If kyyard still cannot
# use the socket, the database CA or the data directory, it keeps root with a warning, so an
# upgrade never stops a working install; with the socket mounted the server is
# host-root-equivalent either way. Started as any other user (--user), it just runs the server.
set -eu
server=/app/kyyard-server
[ "$(id -u)" = 0 ] || exec "$server" "$@"

data=${KY_DATA_DIR:-/data}
sock=${KY_DOCKER_SOCKET-/var/run/docker.sock}
ca=/etc/kyyard-db-ca.crt
mkdir -p "$data"

# own USER: chown everything under the data directories not already USER's. The key files
# must belong to whichever user the server runs as.
own() {
	for dir in "$data" "${KY_BACKUP_DIR:-}"; do
		if [ -z "$dir" ] || [ ! -d "$dir" ]; then continue; fi
		find "$dir" ! -user "$1" -exec chown -h "$1:$1" {} + || true
	done
}
as_kyyard() { su-exec kyyard "$@"; }

if [ -n "$sock" ] && [ -S "$sock" ]; then
	gid=$(stat -c %g "$sock")
	group=$(awk -F: -v gid="$gid" '$3 == gid { print $1; exit }' /etc/group)
	[ -n "$group" ] || { group="docker-$gid"; addgroup -S -g "$gid" "$group"; }
	id -Gn kyyard | tr ' ' '\n' | grep -qxF "$group" || addgroup kyyard "$group"
fi

if { [ -z "$sock" ] || [ ! -S "$sock" ] || as_kyyard test -w "$sock"; } \
	&& { [ ! -e "$ca" ] || as_kyyard test -r "$ca"; }; then
	own kyyard
	as_kyyard test -w "$data" && exec su-exec kyyard "$server" "$@"
fi
echo "kyyard: running as root: user kyyard (uid 10001) cannot use $data, $sock or $ca; see README \"Container user\"" >&2
own root
exec "$server" "$@"
