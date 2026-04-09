#!/bin/bash
#
# Scaffold a new term-llm agent project directory.
#
# Usage: docker/init.sh <agent-name> [output-dir]
#
# Creates a self-contained directory with a docker-compose.yml, .env,
# runit services, and starter agent config. Each agent gets its own
# compose file — run as many agents as you like, each fully independent.
#
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TEMPLATE_DIR="$SCRIPT_DIR/templates"

AGENT_NAME="${1:?Usage: docker/init.sh <agent-name> [output-dir]}"
OUTPUT_DIR="${2:-./$AGENT_NAME}"

# Validate agent name
if [[ ! "$AGENT_NAME" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "Error: agent name must start with a letter, lowercase alphanumeric + hyphens"
  echo "  e.g. 'jarvis', 'my-bot', 'dev-agent'"
  exit 1
fi

if [ -d "$OUTPUT_DIR" ]; then
  echo "Error: $OUTPUT_DIR already exists"
  exit 1
fi

# Generate a random bearer token for the web UI
WEB_TOKEN="$(head -c 24 /dev/urandom | xxd -p)"

# Scaffold directory
mkdir -p "$OUTPUT_DIR/agents/$AGENT_NAME"

# Copy service templates (these are plain shell scripts, no interpolation needed)
cp -r "$TEMPLATE_DIR/services" "$OUTPUT_DIR/services"

# Copy and interpolate init.sh
cp "$TEMPLATE_DIR/init.sh" "$OUTPUT_DIR/init.sh"
chmod +x "$OUTPUT_DIR/init.sh"

# Interpolate agent templates
sed "s/{{AGENT_NAME}}/$AGENT_NAME/g" "$TEMPLATE_DIR/docker-compose.yml" > "$OUTPUT_DIR/docker-compose.yml"
sed "s/{{AGENT_NAME}}/$AGENT_NAME/g" "$TEMPLATE_DIR/agent.yaml" > "$OUTPUT_DIR/agents/$AGENT_NAME/agent.yaml"
sed "s/{{AGENT_NAME}}/$AGENT_NAME/g" "$TEMPLATE_DIR/system.md" > "$OUTPUT_DIR/agents/$AGENT_NAME/system.md"
cp "$TEMPLATE_DIR/soul.md" "$OUTPUT_DIR/agents/$AGENT_NAME/soul.md"

# .env with repo path and generated token
sed -e "s/{{AGENT_NAME}}/$AGENT_NAME/g" \
    -e "s/{{WEB_TOKEN}}/$WEB_TOKEN/" \
    -e "s|^TERM_LLM_REPO=.*|TERM_LLM_REPO=$REPO_DIR|" \
    "$TEMPLATE_DIR/.env.sample" > "$OUTPUT_DIR/.env"

OUTPUT_DIR_ABS="$(cd "$OUTPUT_DIR" && pwd)"

cat <<EOF

  Created agent project: $OUTPUT_DIR_ABS/

  $AGENT_NAME/
  ├── docker-compose.yml
  ├── .env                ← add API keys, token pre-generated
  ├── init.sh             ← boot hook (installs runit services)
  ├── agents/
  │   └── $AGENT_NAME/
  │       ├── agent.yaml
  │       ├── soul.md      ← voice, values, personality
  │       └── system.md    ← operational context
  └── services/
      ├── webui/run           ← web UI on port 8081
      ├── jobs/run            ← job scheduler
      └── bootstrap-jobs/run  ← creates default jobs on first boot

  Next steps:
    1. Edit .env — add at least one API key (ANTHROPIC_API_KEY, etc.)
    2. Edit agents/$AGENT_NAME/soul.md — shape the personality
    3. Edit agents/$AGENT_NAME/system.md — add operational context
    4. cd $OUTPUT_DIR_ABS && docker compose up -d

  Web UI will be at http://localhost:\${WEB_PORT:-8081}/chat

EOF
