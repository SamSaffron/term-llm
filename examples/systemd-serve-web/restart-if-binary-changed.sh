#!/bin/sh

set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: $0 UNIT BINARY" >&2
    exit 2
fi

unit=$1
binary=$2

# An updater may briefly replace a binary non-atomically. A scheduled check
# should fail closed and try again tomorrow rather than degrade the user manager.
if [ ! -x "$binary" ]; then
    echo "term-llm update check: binary is temporarily unavailable: $binary; skipping" >&2
    exit 0
fi

# A scheduled update check should never start a service that its owner stopped.
if ! systemctl --user is-active --quiet "$unit"; then
    echo "term-llm update check: $unit is inactive; nothing to do"
    exit 0
fi

pid=$(systemctl --user show --property=MainPID --value "$unit")
case "$pid" in
    ''|*[!0-9]*|0)
        echo "term-llm update check: $unit has no running main process yet; skipping"
        exit 0
        ;;
esac

running_binary="/proc/$pid/exe"
if [ ! -r "$running_binary" ]; then
    echo "term-llm update check: running executable is unavailable; skipping"
    exit 0
fi

# Most updates change file size, avoiding a full comparison in the common case.
installed_size=$(stat -Lc %s "$binary" 2>/dev/null || true)
running_size=$(stat -Lc %s "$running_binary" 2>/dev/null || true)
if [ -n "$installed_size" ] && [ "$installed_size" = "$running_size" ] && cmp -s "$binary" "$running_binary"; then
    echo "term-llm update check: running binary is current"
    exit 0
fi

# Equal-size binaries still need a byte comparison. If size lookup failed, cmp
# remains authoritative.
if { [ -z "$installed_size" ] || [ -z "$running_size" ]; } && cmp -s "$binary" "$running_binary"; then
    echo "term-llm update check: running binary is current"
    exit 0
fi

# The process may have exited between the status query and comparison. Restart
# still has the desired result: the active service will use the binary on disk.
echo "term-llm update check: binary changed; restarting $unit"
systemctl --user restart "$unit"
