#!/bin/bash
set -e

export AGENT_NAME="${AGENT_NAME:-agent}"
AGENT_DIR="/root/.config/term-llm/agents/$AGENT_NAME"
SEED_DIR="/seed/agents/$AGENT_NAME"

# Bootstrap agent files on first run from seed volume mount
if [ ! -f "$AGENT_DIR/agent.yaml" ] && [ -d "$SEED_DIR" ]; then
  mkdir -p "$AGENT_DIR"
  cp -r "$SEED_DIR"/. "$AGENT_DIR"/
  echo "$AGENT_NAME: bootstrapped from seed"
elif [ ! -f "$AGENT_DIR/agent.yaml" ]; then
  echo "Warning: no agent config and no seed found."
  echo "  expected: $AGENT_DIR/agent.yaml"
  echo "  seed:     $SEED_DIR/"
  echo "Run docker/init.sh to create a project with starter files."
fi

# Seed init hook (from user's project directory, bind-mounted)
if [ -f /seed/init.sh ]; then
  bash /seed/init.sh
fi

# Volume init hook (runs every container start)
if [ -f /root/.config/term-llm/init.sh ]; then
  bash /root/.config/term-llm/init.sh
fi

# No command → boot runit as PID 1
if [ "$#" -eq 0 ]; then
  mkdir -p /etc/runit/runsvdir
  exec runsvdir /etc/runit/runsvdir
fi

exec "$@"
