#!/bin/sh
# A throwaway signpost for the end-to-end tests: a fresh demo database, the
# dashboard on :18281 trusting Remote-User (the tests send it, standing in for
# Authelia), redirects on :18280. Needs ./signpost built with its assets
# (make build).
set -e
cd "$(dirname "$0")/.."
DB=e2e/.data/e2e.db
mkdir -p e2e/.data
rm -f "$DB" "$DB-wal" "$DB-shm"
./signpost demo-seed -db "$DB" -clicks 3000
exec env SIGNPOST_DB="$DB" SIGNPOST_TOKEN=e2e SIGNPOST_TRUST_REMOTE_USER=true \
  SIGNPOST_LISTEN=:18280 SIGNPOST_ADMIN_LISTEN=:18281 SIGNPOST_ADMIN_HOST=links.test \
  ./signpost serve
