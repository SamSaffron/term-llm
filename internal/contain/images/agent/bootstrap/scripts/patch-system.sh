#!/bin/bash
# Safe system.md patcher — generic, uses AGENT_NAME env var.
# Usage: patch-system.sh <new_system_md_file>

set -euo pipefail

AGENT="${AGENT_NAME:-agent}"
AGENT_DIR="/home/agent/.config/term-llm/agents/$AGENT"
SYSTEM_FILE="$AGENT_DIR/system.md"
BACKUP_FILE="$SYSTEM_FILE.bak"
NEW_FILE="${1:-}"

log()  { echo "==> $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

[ -z "$NEW_FILE" ] && fail "Usage: patch-system.sh <path_to_new_system_md>"
[ -f "$NEW_FILE" ] || fail "File not found: $NEW_FILE"

# ── Validation ────────────────────────────────────────────────────────────────

log "Validating proposed system.md for agent=$AGENT..."

SIZE=$(wc -c < "$NEW_FILE")
[ "$SIZE" -gt 100 ] || fail "Proposed system.md is suspiciously small (${SIZE} bytes) — aborting."

# Must contain the agent name as identity anchor
grep -qi "$AGENT" "$NEW_FILE" || fail "Proposed system.md missing '$AGENT' identity anchor — aborting."

# Must reference memory structure
grep -q "memory/" "$NEW_FILE" || fail "Proposed system.md missing memory structure reference — aborting."

# Must contain safety rules
grep -q "patch-agent.sh\|patch-system.sh\|NEVER.*edit.*agent.yaml\|never.*direct" "$NEW_FILE" \
    || fail "Proposed system.md is missing agent safety rules — aborting."

# Bloat check
if [ -f "$SYSTEM_FILE" ]; then
    CURRENT_SIZE=$(wc -c < "$SYSTEM_FILE")
    MAX_SIZE=$(( CURRENT_SIZE * 3 ))
    if [ "$SIZE" -gt "$MAX_SIZE" ]; then
        log "WARNING: new system.md is more than 3x the current size (${SIZE} vs ${CURRENT_SIZE} bytes)."
    fi
fi

log "Validation passed."

# ── Apply ─────────────────────────────────────────────────────────────────────

if [ -f "$SYSTEM_FILE" ]; then
    log "Backing up current system.md → system.md.bak"
    cp "$SYSTEM_FILE" "$BACKUP_FILE"
fi

log "Applying new system.md..."
cp "$NEW_FILE" "$SYSTEM_FILE"

log "Done. Diff from backup:"
[ -f "$BACKUP_FILE" ] && diff "$BACKUP_FILE" "$SYSTEM_FILE" || true

log "If anything broke: cp $BACKUP_FILE $SYSTEM_FILE"
