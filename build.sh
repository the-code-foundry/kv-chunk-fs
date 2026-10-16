#!/bin/bash
# Build bin/kv-chunk-fs in the golang container (no Go toolchain on the host),
# after go vet and the unit tests. Module and build caches live in .go-cache/
# (override with GO_CACHE_DIR to share them between checkouts).
#
#   ./build.sh            # vet, test, build
#   ./build.sh test       # vet and test only
set -euo pipefail
cd "$(dirname "$0")"
GO_IMAGE="golang:1.27-trixie"
CACHE="${GO_CACHE_DIR:-$PWD/.go-cache}"
mkdir -p "$CACHE" bin

go() {
  docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e CGO_ENABLED=0 \
    -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
    -v "$CACHE:/cache" -v "$PWD:/src" -w /src "$GO_IMAGE" go "$@"
}

go mod tidy
go vet ./...
go test -count=1 ./...
[ "${1:-}" = test ] && exit 0
VERSION="$(git describe --always --dirty 2>/dev/null || echo dev)"
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o bin/kv-chunk-fs ./cmd/kv-chunk-fs
echo "[build] bin/kv-chunk-fs $VERSION ($(du -h bin/kv-chunk-fs | cut -f1))"
