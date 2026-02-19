#!/bin/bash
set -e

AGENT_DIR="/root/.config/term-llm/agents/jarvis"

# Bootstrap agent files on first run
if [ ! -f "$AGENT_DIR/agent.yaml" ]; then
  mkdir -p "$AGENT_DIR"
  cp /bootstrap/agent.yaml "$AGENT_DIR/agent.yaml"
  cp /bootstrap/system.md "$AGENT_DIR/system.md"
  echo "Jarvis: bootstrapped agent files"
fi

exec "$@"
