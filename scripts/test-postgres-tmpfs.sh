#!/usr/bin/env bash
# Runs Go tests against a throwaway PostgreSQL that lives entirely on tmpfs.
# Usage: scripts/test-postgres-tmpfs.sh [go test args...]   (defaults: -count=1 -tags=integration ./pkg/runtime)
# It only ever points the tests at its own private socket, never a shared database.
set -euo pipefail
P=$(mktemp -d /tmp/axiom-pg-XXXX)
trap 'pg_ctl -D "$P/data" -m immediate stop >/dev/null 2>&1 || true; rm -rf "$P"' EXIT
initdb -D "$P/data" -U postgres -A trust >/dev/null
pg_ctl -D "$P/data" -o "-p 55432 -k $P -c listen_addresses='' -c fsync=off -c synchronous_commit=off -c full_page_writes=off" -l "$P/log" -w start >/dev/null
export OPENSEAL_TEST_POSTGRES_DSN="host=$P port=55432 user=postgres dbname=postgres sslmode=disable"
export TMPDIR=$P
cd "$(dirname "$0")/.."
if [ $# -eq 0 ]; then set -- -count=1 -tags=integration ./pkg/runtime; fi
go test "$@"
