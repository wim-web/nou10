#!/bin/sh
set -eu
attempt=0
while [ "$attempt" -lt 10 ]; do
  if curl --fail --silent --show-error --max-time 3 http://127.0.0.1:8080/health; then
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 2
done
exit 1
