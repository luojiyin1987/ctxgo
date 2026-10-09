#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/package-release.sh TAG OUTPUT_DIR [GOOS/GOARCH ...]

Build release archives from the current Go module.
The output directory must not exist before the command runs.
The default targets are Linux, macOS, and Windows on amd64 and arm64.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if (( $# < 2 )); then
  usage >&2
  exit 2
fi

tag=$1
output_dir=$2
shift 2
if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Tag must use vX.Y.Z: %s\n' "$tag" >&2
  exit 2
fi
if [[ ! -f go.mod || ! -f LICENSE || ! -f README.md ]]; then
  printf 'Run this script from the repository root.\n' >&2
  exit 2
fi
if [[ -e $output_dir ]]; then
  printf 'Output directory already exists: %s\n' "$output_dir" >&2
  exit 2
fi

if (( $# == 0 )); then
  set -- linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
fi
for target in "$@"; do
  case "$target" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
    *) printf 'Unsupported target: %s\n' "$target" >&2; exit 2 ;;
  esac
done

case "$output_dir" in
  /*) ;;
  *) output_dir=$PWD/$output_dir ;;
esac
mkdir -p "$output_dir"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
archives=()

for target in "$@"; do
  goos=${target%/*}
  goarch=${target#*/}
  name="ctxgo_${tag}_${goos}_${goarch}"
  mkdir "$stage/$name"
  binary=ctxgo
  if [[ $goos == windows ]]; then
    binary=ctxgo.exe
  fi
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' -o "$stage/$name/$binary" .
  cp LICENSE README.md "$stage/$name/"
  if [[ $goos == windows ]]; then
    archive="$name.zip"
    (cd "$stage" && zip -q -r "$output_dir/$archive" "$name")
  else
    archive="$name.tar.gz"
    tar -C "$stage" -czf "$output_dir/$archive" "$name"
  fi
  archives+=("$archive")
  rm -r "$stage/$name"
done

(cd "$output_dir" && sha256sum "${archives[@]}" > SHA256SUMS)
printf 'Release packages: %s\n' "$output_dir"
