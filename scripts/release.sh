#!/usr/bin/env bash
# Build packages locally, or publish already-built packages from the release workflow.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo 'usage: bash scripts/release.sh {build|publish|version} vMAJOR.MINOR.PATCH[-PRERELEASE] [output-dir]' >&2
  exit 1
fi
action=$1
tag=$2
output_dir=${3:-dist}
tag_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if [[ ! $tag =~ $tag_pattern ]]; then
  echo 'release tag must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-PRERELEASE' >&2
  exit 1
fi
version=${tag#v}
if [[ $version == *-* ]]; then
  IFS=. read -r -a identifiers <<< "${version#*-}"
  for identifier in "${identifiers[@]}"; do
    if [[ $identifier =~ ^[0-9]+$ && ${#identifier} -gt 1 && $identifier == 0* ]]; then
      echo 'numeric prerelease identifiers cannot have leading zeros' >&2
      exit 1
    fi
  done
fi

assets=()
for platform in linux darwin; do
  for arch in amd64 arm64; do
    assets+=("nou10_${version}_${platform}_${arch}.tar.gz")
  done
done

checksums() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${assets[@]}"
  else
    shasum -a 256 "${assets[@]}"
  fi
}

case "$action" in
  version)
    printf '%s\n' "$version"
    ;;
  build)
    mkdir -p "$output_dir"
    output_dir=$(cd "$output_dir" && pwd)
    release_tmp=$(mktemp -d "${TMPDIR:-/tmp}/nou10-release.XXXXXX")
    trap 'rm -rf "$release_tmp"' EXIT
    for platform in linux darwin; do
      for arch in amd64 arm64; do
        package_dir="$release_tmp/${platform}_${arch}"
        mkdir -p "$package_dir"
        CGO_ENABLED=0 GOOS="$platform" GOARCH="$arch" \
          go build -trimpath -ldflags "-s -w -X main.version=$version" \
          -o "$package_dir/nou10" ./cmd/nou10
        cp README.md "$package_dir/"
        cp -R docs examples "$package_dir/"
        # COPYFILE_DISABLE prevents macOS metadata entries in local builds.
        COPYFILE_DISABLE=1 tar -czf "$output_dir/nou10_${version}_${platform}_${arch}.tar.gz" \
          -C "$package_dir" nou10 README.md docs examples
      done
    done
    (cd "$output_dir" && checksums > checksums.txt)
    printf 'Built %s packages in %s\n' "$tag" "$output_dir"
    ;;
  publish)
    : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be owner/repository}"
    # Verify exactly the four expected archives before making any GitHub writes.
    # Extra/stale files in the directory are never uploaded.
    (cd "$output_dir" && diff -u checksums.txt <(checksums))
    prerelease=false
    latest=true
    if [[ $version == *-* ]]; then
      prerelease=true
      latest=false
    fi
    # release-please owns versions, tags, changelog and release creation.
    is_draft=$(gh release view "$tag" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft)
    if [[ $is_draft != true ]]; then
      echo 'release is already published; use a new release PR instead of replacing its assets' >&2
      exit 1
    fi
    uploads=()
    for asset in "${assets[@]}" checksums.txt; do
      uploads+=("$output_dir/$asset")
    done
    # A failed upload leaves a draft. A retry may replace draft assets only.
    gh release upload "$tag" "${uploads[@]}" --repo "$GITHUB_REPOSITORY" --clobber
    gh release edit "$tag" --repo "$GITHUB_REPOSITORY" --verify-tag \
      --draft=false --prerelease="$prerelease" --latest="$latest"
    ;;
  *)
    echo "unknown release action: $action" >&2
    exit 1
    ;;
esac
