#!/bin/sh
set -eu

modules="internal/reflow third_party/ultraviolet third_party/bubbletea"

for module in $modules; do
  echo "==> $module: build"
  (cd "$module" && go build ./...)
  echo "==> $module: test"
  (cd "$module" && go test ./...)
  echo "==> $module: vet"
  (cd "$module" && go vet ./...)
  if [ "${VERIFY_RACE:-0}" = "1" ]; then
    echo "==> $module: race"
    (cd "$module" && go test -race ./...)
  fi
done
