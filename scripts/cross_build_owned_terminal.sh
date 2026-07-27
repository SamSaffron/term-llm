#!/bin/sh
set -eu

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  goos=${target%/*}
  goarch=${target#*/}
  echo "==> root: $target"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build ./...
done

for module in internal/terminal/runtime internal/terminal/renderer; do
  for target in linux/amd64 darwin/arm64 windows/amd64; do
    goos=${target%/*}
    goarch=${target#*/}
    echo "==> $module: $target"
    (cd "$module" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build ./...)
  done
done
