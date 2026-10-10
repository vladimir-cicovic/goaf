#!/bin/sh
# External module example: ensure a file holds the wanted content.
# Params (environment): GOAF_P_path (default /tmp/motd-example),
# GOAF_P_content (required).
# Protocol: "$0 check|apply" prints "needed|changed: true|false",
# optional "output: ..." lines, or "error: ..." on failure.
file="${GOAF_P_path:-/tmp/motd-example}"
want="$GOAF_P_content"
verb="$1"

if [ -z "$want" ]; then
  echo "error: content param is required"
  exit 1
fi

cur="$(cat "$file" 2>/dev/null)"
case "$verb" in
  check)
    if [ "$cur" = "$want" ]; then
      echo "needed: false"
    else
      echo "needed: true"
    fi
    ;;
  apply)
    printf '%s' "$want" > "$file" || { echo "error: cannot write $file"; exit 1; }
    echo "changed: true"
    echo "output: updated $file"
    ;;
  *)
    echo "error: want check|apply, got $verb"
    exit 1
    ;;
esac
