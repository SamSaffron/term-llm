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

# If no command given, boot runit as PID 1.
# Services are managed via /etc/runit/runsvdir — symlink /etc/sv/<name> there to enable.
# The volume at /root persists service configs and state across restarts.
if [ "$#" -eq 0 ]; then
  mkdir -p /etc/runit/runsvdir
  exec runsvdir /etc/runit/runsvdir
fi

exec "$@"
