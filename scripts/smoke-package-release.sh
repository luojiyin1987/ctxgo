#!/usr/bin/env bash
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT

"$repo/scripts/package-release.sh" --help > /dev/null
if (cd "$repo" && "$repo/scripts/package-release.sh" invalid "$temp/invalid" linux/amd64 > /dev/null 2>&1); then
  printf 'Invalid release tag was accepted.\n' >&2
  exit 1
fi

(cd "$repo" && "$repo/scripts/package-release.sh" v0.0.0 "$temp/dist" linux/amd64)
(cd "$temp/dist" && sha256sum -c SHA256SUMS)
mkdir "$temp/extracted"
tar -C "$temp/extracted" -xzf "$temp/dist/ctxgo_v0.0.0_linux_amd64.tar.gz"
"$temp/extracted/ctxgo_v0.0.0_linux_amd64/ctxgo" help > /dev/null
test -f "$temp/extracted/ctxgo_v0.0.0_linux_amd64/LICENSE"
test -f "$temp/extracted/ctxgo_v0.0.0_linux_amd64/README.md"
printf 'release package smoke test OK\n'
