#!/bin/sh
set -eu
version=${1:-dev}
case "$version" in ''|*[!A-Za-z0-9._-]*) echo 'Invalid release version' >&2; exit 1 ;; esac
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=${2:-"$repo/dist"}
case "$out" in /*) ;; *) out="$(pwd)/$out" ;; esac
revision=$(git -c safe.directory="$repo" -C "$repo" rev-parse --short=12 HEAD)
if [ -n "$(git -c safe.directory="$repo" -C "$repo" status --porcelain --untracked-files=normal)" ]; then
  revision="$revision+working"
fi
SOURCE_DATE_EPOCH=$(git -c safe.directory="$repo" -C "$repo" show -s --format=%ct HEAD)
export SOURCE_DATE_EPOCH
mkdir -p "$out"
cd "$repo"
for target in linux/amd64 linux/arm64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}; name="cyapi-$os-$arch"
  if [ "$os" = windows ]; then name="$name.exe"; fi
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$revision" -o "$out/$name" ./cmd/cyapi
done
python3 "$repo/scripts/package-release.py" "$out"
