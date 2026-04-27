#!/bin/bash
# Self-update script for term-llm — generic, works for any agent.
# Clones/pulls latest, compiles, installs, restarts runit services.

set -e

REPO="https://github.com/samsaffron/term-llm.git"
SRC_DIR="$HOME/source/term-llm"
BINARY_DEST="/usr/local/bin/term-llm"
BEFORE=$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || echo "unknown")

log() { echo "==> $*"; }

# Clone or update source
if [ -d "$SRC_DIR/.git" ]; then
    log "Pulling latest changes..."
    # Use 'upstream' if it exists, otherwise 'origin'
    REMOTE="origin"
    git -C "$SRC_DIR" remote | grep -q "^upstream$" && REMOTE="upstream"
    git -C "$SRC_DIR" fetch "$REMOTE"
    git -C "$SRC_DIR" checkout main
    git -C "$SRC_DIR" reset --hard "$REMOTE/main"
else
    log "Cloning term-llm from GitHub..."
    rm -rf "$SRC_DIR"
    git clone --depth=1 "$REPO" "$SRC_DIR"
fi

cd "$SRC_DIR"

COMMIT=$(git rev-parse --short HEAD)

if [ "$BEFORE" = "$COMMIT" ]; then
    log "Already up to date ($COMMIT) — rebuilding anyway"
else
    log "Updated $BEFORE -> $COMMIT"
fi
VERSION=$(git describe --tags --abbrev=0 2>/dev/null || echo "dev")
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

log "Building version=$VERSION commit=$COMMIT date=$BUILD_DATE"

go build \
    -ldflags "-s -w \
        -X github.com/samsaffron/term-llm/cmd.Version=${VERSION} \
        -X github.com/samsaffron/term-llm/cmd.Commit=${COMMIT} \
        -X github.com/samsaffron/term-llm/cmd.Date=${BUILD_DATE}" \
    -o /tmp/term-llm-new \
    ./main.go

log "Installing binary to $BINARY_DEST..."
sudo install -m 755 /tmp/term-llm-new "$BINARY_DEST"
rm -f /tmp/term-llm-new

log "Done! Installed:"
"$BINARY_DEST" version

# Restart runit-managed services that use term-llm
if command -v sv >/dev/null 2>&1 && [ -d /etc/runit/runsvdir ]; then
    for svc in /etc/runit/runsvdir/*/; do
        svc="${svc%/}"
        svc_name="$(basename "$svc")"
        run_script="/etc/sv/${svc_name}/run"
        if ! grep -q "term-llm" "$run_script" 2>/dev/null; then
            log "Skipping (no term-llm): $svc_name"
            continue
        fi
        if ! sudo sv status "$svc" 2>/dev/null | grep -q "^run:"; then
            log "Skipping (not running): $svc_name"
            continue
        fi
        if [ "$svc_name" = "jobs" ]; then
            log "Scheduling detached SIGKILL restart in 3s: $svc_name"
            setsid nohup bash -c "
                sleep 3
                sudo sv stop '$svc' 2>/dev/null || true
                sleep 1
                if [ -f '$svc/supervise/pid' ]; then
                    sudo kill -9 \$(cat '$svc/supervise/pid') 2>/dev/null || true
                fi
                sudo sv start '$svc'
            " >/dev/null 2>&1 &
        else
            log "Scheduling detached restart in 3s: $svc_name"
            setsid nohup bash -c "sleep 3 && sudo sv restart '$svc'" >/dev/null 2>&1 &
        fi
    done
    log "All restarts scheduled — services will cycle in ~3s"
fi
