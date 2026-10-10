#!/bin/sh
# Dynamic inventory from local docker: every running container publishing
# a host port to container port 22 becomes a host in the "docker" group.
# Usage: goaf -i "exec:./docker-inventory.sh" ...
# Env: GOAF_USER (default root), GOAF_HOST (default 127.0.0.1, use the
# docker host address when the daemon is remote).
set -u

USER="${GOAF_USER:-root}"
HOST="${GOAF_HOST:-127.0.0.1}"

echo "groups:"
echo "  docker:"
echo "    hosts:"

docker ps --format '{{.Names}} {{.Ports}}' | while read -r name ports; do
  # ports looks like: 0.0.0.0:2222->22/tcp, :::2222->22/tcp
  port=$(printf '%s' "$ports" | tr ',' '\n' | grep -- '->22/tcp' | head -1 | sed 's/.*:\([0-9][0-9]*\)->22\/tcp.*/\1/')
  if [ -n "$port" ]; then
    echo "      - ${HOST}:${port} # ${name}"
  fi
done

echo "vars:"
echo "  user: ${USER}"
