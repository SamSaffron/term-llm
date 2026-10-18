#!/bin/sh
# Run the checks relevant to the current change. See `scripts/check-changed.sh --help`.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
exec go run ./scripts/checkchanged "$@"
