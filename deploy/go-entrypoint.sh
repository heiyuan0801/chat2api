#!/bin/sh
set -eu
umask 077
if [ -e /app/data/.restore-incomplete ]; then
  echo 'Incomplete restore: keep this deployment stopped and restore into fresh volumes.' >&2
  exit 1
fi
exec control-server -listen 0.0.0.0:8080 -data-dir /app/data \
  -web-dir /opt/chatgpt2api/web_dist "$@"
