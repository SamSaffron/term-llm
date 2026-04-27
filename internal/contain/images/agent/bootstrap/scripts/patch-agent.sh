#!/bin/bash
# Safe agent.yaml patcher — generic, uses AGENT_NAME env var.
# Usage: patch-agent.sh <yaml_content_file>

set -euo pipefail

AGENT="${AGENT_NAME:-agent}"
AGENT_DIR="/home/agent/.config/term-llm/agents/$AGENT"
AGENT_FILE="$AGENT_DIR/agent.yaml"
BACKUP_FILE="$AGENT_FILE.bak"
NEW_FILE="${1:-}"

log()  { echo "==> $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

[ -z "$NEW_FILE" ] && fail "Usage: patch-agent.sh <path_to_new_yaml>"
[ -f "$NEW_FILE" ] || fail "File not found: $NEW_FILE"

# ── Validation ────────────────────────────────────────────────────────────────

log "Validating proposed agent.yaml for agent=$AGENT..."

if command -v python3 &>/dev/null; then
    python3 -c "
import sys, yaml
try:
    data = yaml.safe_load(open('$NEW_FILE'))
except yaml.YAMLError as e:
    print('YAML parse error:', e)
    sys.exit(1)

required = ['name', 'tools', 'max_turns']
for k in required:
    if k not in data:
        print(f'Missing required key: {k}')
        sys.exit(1)

enabled = data.get('tools', {}).get('enabled', None)
if not isinstance(enabled, list):
    print('tools.enabled must be a list')
    sys.exit(1)

mt = data.get('max_turns', 0)
if not isinstance(mt, int) or mt <= 0:
    print('max_turns must be a positive integer')
    sys.exit(1)

print('Validation passed.')
" || fail "Proposed agent.yaml failed validation — aborting, original untouched."
else
    grep -q "^name:" "$NEW_FILE"        || fail "Missing 'name:' field"
    grep -q "max_turns:" "$NEW_FILE"    || fail "Missing 'max_turns:' field"
    grep -q "enabled:" "$NEW_FILE"      || fail "Missing 'tools.enabled' field"
    log "Basic grep checks passed (python3 not available for full YAML parse)."
fi

# ── Apply ─────────────────────────────────────────────────────────────────────

if [ -f "$AGENT_FILE" ]; then
    log "Backing up current agent.yaml → agent.yaml.bak"
    cp "$AGENT_FILE" "$BACKUP_FILE"
fi

log "Applying new agent.yaml..."
cp "$NEW_FILE" "$AGENT_FILE"

log "Done. Diff from backup:"
[ -f "$BACKUP_FILE" ] && diff "$BACKUP_FILE" "$AGENT_FILE" || true

log "If anything broke: cp $BACKUP_FILE $AGENT_FILE"
