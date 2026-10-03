#!/bin/sh
set -eu

docker --config /etc/myapp/docker compose up -d --build --wait
