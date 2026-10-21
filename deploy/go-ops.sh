#!/usr/bin/env bash
# Offline operations for the dedicated PostgreSQL/Redis Go Compose deployment.
set -euo pipefail
umask 077
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
env_file=${CHATGPT2API_GO_ENV_FILE:-$root/.env.go}
compose=(docker compose --project-directory "$root" --env-file "$env_file" -f "$root/docker-compose.go.yml")
die() { echo "$*" >&2; exit 1; }
hashes() {
  if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}
dc() { "${compose[@]}" "$@"; }
tool() { dc run --rm --no-deps -T --entrypoint sh app "$@"; }
running() { test -n "$(dc ps --status running -q app)"; }
check_backup() {
  [[ $1 = /* && -d $1 ]] || die 'An absolute backup directory is required.'
  for name in application-database.pgdump data.tar.gz VERSION image-id.txt checksums.txt; do
    [[ -f $1/$name && ! -L $1/$name ]] || die 'Incomplete backup directory.'
  done
  [[ $(wc -l < "$1/checksums.txt" | tr -d ' ') = 4 ]] || die 'Invalid checksum manifest.'
  # Only these literal names may be read by the checksum tool.
  awk 'NF != 2 || $1 !~ /^[a-f0-9]+$/ || length($1) != 64 || $2 !~ /^(application-database.pgdump|data.tar.gz|VERSION|image-id.txt)$/ {exit 1} {if (seen[$2]++) exit 1}' "$1/checksums.txt" || die 'Invalid checksum manifest.'
  (cd "$1" && hashes -c checksums.txt) || die 'Backup checksum verification failed.'
  dc exec -T postgres pg_restore --list < "$1/application-database.pgdump" >/dev/null
  # GNU tar escapes unusual names; reject links, devices, absolute and parent paths.
  tool -ec '
    cat > /tmp/backup-data.tar.gz
    tar -tzf /tmp/backup-data.tar.gz > /tmp/backup-paths
    awk '\''/^\// || /(^|\/)\.\.(\/|$)/ || /\\/ {exit 1}'\'' /tmp/backup-paths
    tar -tvzf /tmp/backup-data.tar.gz > /tmp/backup-types
    awk '\''substr($0,1,1) != "-" && substr($0,1,1) != "d" {exit 1}'\'' /tmp/backup-types
  ' < "$1/data.tar.gz"
}
case ${1:-} in
  verify)
    dc exec -T app control-server -healthcheck-url http://127.0.0.1:8080/api/settings
    dc exec -T postgres sh -ec 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"' >/dev/null
    [[ $(dc exec -T redis redis-cli ping | tr -d '\r') = PONG ]] || die 'Redis health check failed.'
    echo 'Go HTTP, PostgreSQL and Redis checks passed.'
    ;;
  backup)
    destination=${2:-}
    [[ $destination = /* && ! -e $destination ]] || die 'Choose a new absolute backup directory.'
    mkdir -m 700 "$destination"
    container=$(dc ps -a -q app)
    [[ -n $container ]] || die 'The application container must exist before backup.'
    image=$(docker inspect --format '{{.Image}}' "$container")
    restart=false
    if running; then restart=true; dc stop app; fi
    # Backup is read-only. On failure the original app may safely resume.
    cleanup() { if $restart; then dc start app >/dev/null; fi; }
    trap cleanup EXIT
    dc exec -T postgres sh -ec 'pg_dump --format=custom --no-owner --no-privileges -U "$POSTGRES_USER" "$POSTGRES_DB"' > "$destination/application-database.pgdump"
    tool -ec 'tar --exclude=./files/.staging --exclude=./.restore-incomplete -C /app/data -czf - .' > "$destination/data.tar.gz"
    docker run --rm --entrypoint cat "$image" /opt/chatgpt2api/VERSION > "$destination/VERSION"
    printf '%s\n' "$image" > "$destination/image-id.txt"
    (cd "$destination" && hashes application-database.pgdump data.tar.gz VERSION image-id.txt > checksums.txt)
    check_backup "$destination"
    echo 'Backup verified; protect this directory as private user data.'
    ;;
  restore)
    source=${2:-}
    running && die 'Stop the target app before restoring. Use a fresh project and volumes.'
    dc up -d --wait postgres redis
    check_backup "$source"
    count=$(dc exec -T postgres sh -ec 'psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('\''pg_catalog'\'','\''information_schema'\'');"')
    [[ $count = 0 ]] || die 'Restore refuses a nonempty database.'
    tool -ec 'test -z "$(find /app/data -mindepth 1 -maxdepth 1 -print -quit)"' || die 'Restore refuses a nonempty data volume.'
    # A later failure leaves this marker; the container entry point refuses startup.
    tool -ec ': > /app/data/.restore-incomplete'
    dc exec -T postgres sh -ec 'pg_restore --exit-on-error --single-transaction --no-owner --no-privileges -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < "$source/application-database.pgdump"
    tool -ec 'tar --no-same-owner --no-same-permissions --keep-old-files -xzf - -C /app/data' < "$source/data.tar.gz"
    dc exec -T redis redis-cli FLUSHDB >/dev/null
    tool -ec 'rm /app/data/.restore-incomplete'
    echo 'Restore completed; app remains stopped. Start it and verify identities, counts and file hashes.'
    ;;
  rollback)
    source=${2:-}
    check_backup "$source"
    image=$(cat "$source/image-id.txt")
    [[ $image =~ ^sha256:[a-f0-9]{64}$ ]] || die 'Invalid previous image ID.'
    [[ $(docker image inspect --format '{{index .Config.Labels "io.chatgpt2api.runtime"}}' "$image") = go ]] || die 'Previous local Go image is unavailable.'
    dc stop app
    CHATGPT2API_GO_IMAGE=$image "${compose[@]}" up -d --no-deps --no-build --pull never --wait app
    echo 'Previous Go image restored; compatible data retained. Pin this image in the deployment environment.'
    ;;
  *) die 'Usage: deploy/go-ops.sh verify | backup /absolute/new-directory | restore /absolute/backup-directory | rollback /absolute/backup-directory' ;;
esac
