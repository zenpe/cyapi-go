#!/bin/sh
set -eu
version=${1:-dev}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=${2:-"$repo/dist"}
revision=$(git -C "$repo" rev-parse --short HEAD)
mkdir -p "$out"
case "$out" in /*) ;; *) out="$(pwd)/$out" ;; esac
cd "$repo"
for target in linux/amd64 linux/arm64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}; name="cyapi-$os-$arch"
  if [ "$os" = windows ]; then name="$name.exe"; fi
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$revision" -o "$out/$name" ./cmd/cyapi
done
(cd "$out" && sha256sum cyapi-linux-amd64 cyapi-linux-arm64 cyapi-windows-amd64.exe > SHA256SUMS)
