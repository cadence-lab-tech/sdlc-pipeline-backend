#!/usr/bin/env bash
#
# Moves the data of the bundled database from PostgreSQL 17 to PostgreSQL 18
# (docs/admin.md, "PostgreSQL 18"). Not run by hand: docker-compose.yml runs
# it on every `docker compose up` as two one-off services, before postgres
# starts.
#
#   dump  in pg-upgrade-dump, the image of PostgreSQL 17: the old volume
#         (pgdata) at /var/lib/postgresql/data, the new one (postgres) at
#         /target. When the old volume holds a cluster that was not moved yet,
#         starts PostgreSQL 17 on it with no network, dumps every database and
#         role with pg_dumpall into /target/cadence-upgrade/pending and stops
#         it. Otherwise does nothing.
#   load  in pg-upgrade-load, the image of PostgreSQL 18: the new volume at
#         /var/lib/postgresql. When a dump is pending, creates a new cluster
#         next to the data directory, loads the dump into it, and only then
#         puts it in place of the data directory (/var/lib/postgresql/18/docker)
#         and writes the mark of the move - last. Otherwise does nothing.
#
# A fresh installation has no old cluster: both do nothing, and postgres
# creates its cluster as the image always does. The old volume is only read:
# PostgreSQL 17 starts on it and stops, its data stays as it was, and the
# rollback to the previous version runs on it. The mark is the state of the
# old cluster after the dump (pg_controldata): when the old cluster changes
# after the move - a rollback started the previous version on it and wrote
# to it - the next `up` moves it again; the cluster of the earlier move is
# not deleted but kept aside in /var/lib/postgresql/cadence-upgrade/replaced-*
# of the new volume. A move that did not finish (out of space, interrupted)
# leaves no mark: the next `up` starts it over.
#
# Exit codes: 0 done or nothing to do, 1 the move failed - postgres does not
# start, and `docker compose up` fails with it.
set -euo pipefail

log() { printf 'pg-upgrade: %s\n' "$*"; }
die() {
  printf 'pg-upgrade: ✗ %s\n' "$*" >&2
  exit 1
}

# state DIR: what identifies the contents of the cluster in DIR, stopped.
# Every change of the data moves the checkpoint.
state() {
  LC_ALL=C pg_controldata -D "$1" |
    grep -E '^(Database system identifier|Latest checkpoint location|Database cluster state):'
}

# The temporary server listens on a socket of its own, never on the network.
socket=""
server=""
moving=false
start_server() {
  server="$1"
  socket="$(mktemp -d)"
  chown postgres:postgres "$socket"
  gosu postgres pg_ctl -D "$server" -w -t 3600 -l "$socket/server.log" \
    -o "-c listen_addresses='' -c unix_socket_directories='$socket'" start >/dev/null ||
    { cat "$socket/server.log" >&2; die "PostgreSQL did not start on $server"; }
}
stop_server() {
  [ -n "$server" ] || return 0
  gosu postgres pg_ctl -D "$server" -w -t 3600 -m fast stop >/dev/null
  server=""
}
# shellcheck disable=SC2329 # invoked by the EXIT trap
on_exit() {
  local rc=$?
  if [ -n "$server" ]; then
    gosu postgres pg_ctl -D "$server" -w -t 600 -m fast stop >/dev/null 2>&1 || true
  fi
  [ "$rc" = 0 ] || [ "$moving" = false ] ||
    log "the move failed; the old volume is as it was, and the next 'docker compose up' starts the move over"
}
trap on_exit EXIT

dump() {
  local old=/var/lib/postgresql/data work=/target/cadence-upgrade
  local now version user ident
  if [ ! -s "$old/PG_VERSION" ]; then
    log "no PostgreSQL $PG_MAJOR data in the old volume: nothing to move"
    return 0
  fi
  version="$(cat "$old/PG_VERSION")"
  [ "$version" = "$PG_MAJOR" ] ||
    die "the old volume holds PostgreSQL $version, this image is PostgreSQL $PG_MAJOR"
  now="$(state "$old")"
  if [ -f "$work/done" ] && [ "$(cat "$work/done")" = "$now" ]; then
    log "the data of PostgreSQL $PG_MAJOR was moved already and has not changed since: nothing to do"
    return 0
  fi
  if [ -f "$work/pending/ready" ] && [ "$(cat "$work/pending/state")" = "$now" ]; then
    log "the dump of PostgreSQL $PG_MAJOR is there already; it is loaded next"
    return 0
  fi
  # Two servers on one data directory destroy it. On an upgrade compose stops
  # the old postgres container before this one starts (its image changes);
  # if a PostgreSQL still answers, nothing is touched.
  if pg_isready -q -h postgres -p 5432 -t 3 >/dev/null 2>&1; then
    if [ -f "$work/done" ]; then
      # PostgreSQL 18 runs, and the old cluster changed after the move.
      die "the old volume changed after its data was moved, and PostgreSQL 18 runs on the moved data. Nothing is touched.
  If the previous version wrote to it after a rollback and you upgrade again, stop postgres (docker compose stop postgres) and run docker compose up -d: the data is moved again, and the current PostgreSQL 18 cluster is kept aside.
  To keep PostgreSQL 18 as it is, remove the old volume (docs/admin.md, \"PostgreSQL 18\")."
    fi
    die "a PostgreSQL answers at postgres:5432: stop it first (docker compose stop postgres), then run docker compose up -d again"
  fi

  moving=true
  log "moving the data of PostgreSQL $PG_MAJOR to PostgreSQL 18: dumping it (the old volume stays as it is)"
  rm -rf "$work/pending"
  mkdir -p "$work/pending"
  start_server "$old"
  user="${POSTGRES_USER:-postgres}"
  # The role that created the cluster: the new cluster is created by it too.
  ident="$(gosu postgres psql -X -At -h "$socket" -U "$user" -d postgres -c 'SELECT quote_ident(rolname) FROM pg_roles WHERE oid = 10')"
  user="$(gosu postgres psql -X -At -h "$socket" -U "$user" -d postgres -c 'SELECT rolname FROM pg_roles WHERE oid = 10')"
  [ -n "$user" ] || die "cannot tell the role that created the cluster"
  log "$(gosu postgres psql -X -At -h "$socket" -U "$user" -d postgres -c "SELECT count(*) || ' database(s), ' || pg_size_pretty(sum(pg_database_size(oid))) FROM pg_database WHERE datallowconn")"
  # That role exists in the new cluster already: its CREATE ROLE - in the
  # roles at the top, before the first \connect - would fail the load, and
  # its ALTER ROLE that follows carries its attributes and password.
  gosu postgres pg_dumpall -h "$socket" -U "$user" |
    awk -v skip="CREATE ROLE $ident;" 'BEGIN { roles = 1 } /^\\connect / { roles = 0 } !(roles && $0 == skip)' |
    gzip -1 >"$work/pending/dump.sql.gz.part"
  stop_server
  cp "$old/pg_hba.conf" "$work/pending/pg_hba.conf"
  printf '%s\n' "$user" >"$work/pending/user"
  state "$old" >"$work/pending/state"
  mv "$work/pending/dump.sql.gz.part" "$work/pending/dump.sql.gz"
  sync
  : >"$work/pending/ready"
  log "dumped: $(du -h "$work/pending/dump.sql.gz" | cut -f1) compressed"
}

load() {
  local root=/var/lib/postgresql work=/var/lib/postgresql/cadence-upgrade
  local staging user
  : "${PGDATA:?the image sets PGDATA}"
  if [ ! -f "$work/pending/ready" ]; then
    log "no dump to load: nothing to do"
    return 0
  fi
  user="$(cat "$work/pending/user")"
  staging="$work/staging"
  moving=true
  log "loading the dump into a new PostgreSQL $PG_MAJOR cluster"
  rm -rf "$staging"
  mkdir -p "$staging"
  chown postgres:postgres "$root" "$work" "$staging"
  chmod 700 "$staging"
  # As the entrypoint of the image creates a cluster: the same role, the
  # locale of the image, POSTGRES_INITDB_ARGS. The password and the rules of
  # access are those of the old cluster: its pg_hba.conf, and the password in
  # the dump.
  # shellcheck disable=SC2086 # POSTGRES_INITDB_ARGS is a list of arguments
  gosu postgres initdb -D "$staging" --username="$user" --auth=trust ${POSTGRES_INITDB_ARGS:-} >/dev/null
  install -o postgres -g postgres -m 600 "$work/pending/pg_hba.conf" "$staging/pg_hba.conf"
  start_server "$staging"
  gunzip -c "$work/pending/dump.sql.gz" |
    gosu postgres psql -X -q -v ON_ERROR_STOP=1 -h "$socket" -U "$user" -d postgres >/dev/null
  stop_server
  # The data directory now - a cluster of an earlier move whose old cluster
  # has changed since, or one postgres created while no move was done - is
  # kept aside, not deleted: it may hold data written after it.
  if [ -e "$PGDATA" ]; then
    local aside
    aside="$work/replaced-$(date -u +%Y%m%d-%H%M%S)"
    mv "$PGDATA" "$aside"
    log "the cluster that was in $PGDATA is kept in $aside (docs/admin.md, \"PostgreSQL 18\")"
  fi
  mkdir -p "$(dirname "$PGDATA")"
  chown postgres:postgres "$(dirname "$PGDATA")"
  mv "$staging" "$PGDATA"
  sync
  cp "$work/pending/state" "$work/done.part"
  mv "$work/done.part" "$work/done"
  rm -rf "$work/pending"
  log "PostgreSQL $PG_MAJOR has the data now; the old volume is kept for a rollback (docs/admin.md, \"PostgreSQL 18\")"
}

case "${1:-}" in
dump) dump ;;
load) load ;;
*) die "usage: major-upgrade.sh dump|load" ;;
esac
