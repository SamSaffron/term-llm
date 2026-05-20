#!/bin/bash
# Safe soul.md patcher — generic, uses AGENT_NAME env var.
# Usage: patch-soul.sh <new_soul_md_file>

set -euo pipefail

AGENT="${AGENT_NAME:-agent}"
AGENT_DIR="/home/agent/.config/term-llm/agents/$AGENT"
SOUL_FILE="$AGENT_DIR/soul.md"
BACKUP_FILE="$SOUL_FILE.bak"
NEW_FILE="${1:-}"

log()  { echo "==> $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

[ -z "$NEW_FILE" ] && fail "Usage: patch-soul.sh <path_to_new_soul_md>"
[ -f "$NEW_FILE" ] || fail "File not found: $NEW_FILE"

# ── Validation ────────────────────────────────────────────────────────────────

log "Validating proposed soul.md for agent=$AGENT..."

SIZE=$(wc -c < "$NEW_FILE")
[ "$SIZE" -gt 100 ] || fail "Proposed soul.md is suspiciously small (${SIZE} bytes) — aborting."

# Must look like an identity/voice file, not an empty operational stub.
grep -qi "soul\|voice\|personality\|values\|tone\|identity" "$NEW_FILE" \
    || fail "Proposed soul.md is missing an identity/voice anchor — aborting."

# Should not become a dumping ground for operational state.
if grep -qi "API key\|token:\|password\|secret" "$NEW_FILE"; then
    fail "Proposed soul.md appears to contain secrets or secret-like operational data — aborting."
fi

# Bloat check
if [ -f "$SOUL_FILE" ]; then
    CURRENT_SIZE=$(wc -c < "$SOUL_FILE")
    MAX_SIZE=$(( CURRENT_SIZE * 3 ))
    if [ "$SIZE" -gt "$MAX_SIZE" ]; then
        log "WARNING: new soul.md is more than 3x the current size (${SIZE} vs ${CURRENT_SIZE} bytes)."
    fi
fi

log "Validation passed."

# ── Apply ─────────────────────────────────────────────────────────────────────

if [ -f "$SOUL_FILE" ]; then
    log "Backing up current soul.md → soul.md.bak"
    cp "$SOUL_FILE" "$BACKUP_FILE"
fi

log "Applying new soul.md..."
cp "$NEW_FILE" "$SOUL_FILE"

log "Done. Diff from backup:"
[ -f "$BACKUP_FILE" ] && diff "$BACKUP_FILE" "$SOUL_FILE" || true

log "If anything broke: cp $BACKUP_FILE $SOUL_FILE"
