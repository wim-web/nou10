#!/usr/bin/env bash
# Check release failure paths without publishing anything or using credentials.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
release_test_dir=$(mktemp -d "${TMPDIR:-/tmp}/nou10-release-test.XXXXXX")
trap 'rm -rf "$release_test_dir"' EXIT
mkdir -p "$release_test_dir/bin" "$release_test_dir/packages"

for tag in v0.1.0 v1.2.3-rc.1 v2.0.0-beta; do
  [[ $(bash scripts/release.sh version "$tag") == "${tag#v}" ]]
done
for tag in '' latest 1.2.3 v01.2.3 v1.2 v1.2.3-01 v1.2.3-rc.01 'v1.2.3;echo bad'; do
  if bash scripts/release.sh version "$tag" >/dev/null 2>&1; then
    echo "accepted invalid release tag: $tag" >&2
    exit 1
  fi
done

# The mock records argv before interpreting it; no network is used.
cat > "$release_test_dir/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$NOU10_RELEASE_TEST_LOG"
case "$2" in
  view)
    case "$NOU10_RELEASE_TEST_STATE" in
      missing) exit 1 ;;
      published) echo false ;;
      draft) echo true ;;
    esac
    ;;
  upload)
    if [[ ${NOU10_RELEASE_TEST_UPLOAD_FAIL:-false} == true ]]; then exit 1; fi
    ;;
esac
MOCK
chmod +x "$release_test_dir/bin/gh"
export PATH="$release_test_dir/bin:$PATH"
export GITHUB_REPOSITORY=owner/repo
export NOU10_RELEASE_TEST_LOG="$release_test_dir/calls"

make_packages() {
  local tag=$1
  local names=()
  for platform in linux darwin; do
    for arch in amd64 arm64; do
      local name="nou10_${tag#v}_${platform}_${arch}.tar.gz"
      printf 'fixture\n' > "$release_test_dir/packages/$name"
      names+=("$name")
    done
  done
  (
    cd "$release_test_dir/packages"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum "${names[@]}" > checksums.txt
    else
      shasum -a 256 "${names[@]}" > checksums.txt
    fi
  )
  : > "$NOU10_RELEASE_TEST_LOG"
}

make_packages v0.1.0
export NOU10_RELEASE_TEST_STATE=missing
if bash scripts/release.sh publish v0.1.0 "$release_test_dir/packages" >/dev/null 2>&1; then
  echo 'release publisher created a release outside release-please' >&2; exit 1
fi
[[ $(wc -l < "$NOU10_RELEASE_TEST_LOG") -eq 1 ]]
: > "$NOU10_RELEASE_TEST_LOG"
export NOU10_RELEASE_TEST_STATE=draft
bash scripts/release.sh publish v0.1.0 "$release_test_dir/packages"
[[ $(wc -l < "$NOU10_RELEASE_TEST_LOG") -eq 3 ]]
grep -q '^release edit .*--draft=false --prerelease=false --latest=true' "$NOU10_RELEASE_TEST_LOG"

# A partially uploaded draft can be resumed. An upload failure must not publish it.
: > "$NOU10_RELEASE_TEST_LOG"
export NOU10_RELEASE_TEST_STATE=draft NOU10_RELEASE_TEST_UPLOAD_FAIL=true
if bash scripts/release.sh publish v0.1.0 "$release_test_dir/packages" >/dev/null 2>&1; then
  echo 'upload failure was ignored' >&2; exit 1
fi
if grep -q '^release edit' "$NOU10_RELEASE_TEST_LOG"; then
  echo 'failed upload published a release' >&2; exit 1
fi
: > "$NOU10_RELEASE_TEST_LOG"
export NOU10_RELEASE_TEST_UPLOAD_FAIL=false
bash scripts/release.sh publish v0.1.0 "$release_test_dir/packages"
[[ $(wc -l < "$NOU10_RELEASE_TEST_LOG") -eq 3 ]]
grep -q '^release upload .*--clobber' "$NOU10_RELEASE_TEST_LOG"

: > "$NOU10_RELEASE_TEST_LOG"
export NOU10_RELEASE_TEST_STATE=published
if bash scripts/release.sh publish v0.1.0 "$release_test_dir/packages" >/dev/null 2>&1; then
  echo 'published release was accepted for replacement' >&2; exit 1
fi
[[ $(wc -l < "$NOU10_RELEASE_TEST_LOG") -eq 1 ]]

make_packages v0.2.0-rc.1
export NOU10_RELEASE_TEST_STATE=draft
bash scripts/release.sh publish v0.2.0-rc.1 "$release_test_dir/packages"
grep -q '^release edit .*--draft=false --prerelease=true --latest=false' "$NOU10_RELEASE_TEST_LOG"

# Bad artifacts must fail before even looking up a release.
: > "$NOU10_RELEASE_TEST_LOG"
printf 'tampered\n' >> "$release_test_dir/packages/nou10_0.2.0-rc.1_linux_amd64.tar.gz"
if bash scripts/release.sh publish v0.2.0-rc.1 "$release_test_dir/packages" >/dev/null 2>&1; then
  echo 'corrupt package was accepted' >&2; exit 1
fi
[[ ! -s $NOU10_RELEASE_TEST_LOG ]]
echo 'Release validation, draft retries, upload failures and published-release protection passed.'
