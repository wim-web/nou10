#!/bin/sh
# Native Go deployment tests in a disposable Linux container, without credentials.
set -eu
cd "$(dirname "$0")/.."
arch=${NOU10_TEST_ARCH:-$(go env GOARCH)}
case "$arch" in arm64|amd64) ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/nou10-integration.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT HUP INT TERM
for package in host agent; do
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c -o "$test_dir/$package.test" "./internal/$package"
done
chmod 0755 "$test_dir" "$test_dir/host.test" "$test_dir/agent.test"
docker run --rm --platform "linux/$arch" -v "$test_dir:/artifacts:ro" ubuntu:24.04 sh -ec \
  'apt-get update -qq; apt-get install -y -qq --no-install-recommends git ca-certificates; setpriv --reuid=65534 --regid=65534 --clear-groups /artifacts/host.test -test.v; setpriv --reuid=65534 --regid=65534 --clear-groups /artifacts/agent.test -test.run "^TestNativeAgentIntegration$" -test.v'
