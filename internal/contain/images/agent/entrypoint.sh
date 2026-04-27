#!/bin/bash
set -euo pipefail

export AGENT_NAME="${AGENT_NAME:-agent}"
AGENT_USER="agent"
AGENT_HOME="/home/agent"
CONFIG_DIR="$AGENT_HOME/.config/term-llm"
AGENT_DIR="$CONFIG_DIR/agents/$AGENT_NAME"
BOOTSTRAP_SENTINEL="$CONFIG_DIR/.seed-bootstrapped"
IMAGE_BOOTSTRAP_DIR="/opt/term-llm/bootstrap"
SEED_BOOTSTRAP_DIR="/seed"
BOOTSTRAP_DIR="$IMAGE_BOOTSTRAP_DIR"

# /seed is optional and static. If mounted, it may replace the image defaults,
# but it is only read during first-boot bootstrap. It is never consulted again
# after $BOOTSTRAP_SENTINEL exists.
if [ -f "$SEED_BOOTSTRAP_DIR/bootstrap.yaml" ]; then
  BOOTSTRAP_DIR="$SEED_BOOTSTRAP_DIR"
fi

render_file_once() {
  local src="$1"
  local dst="$2"
  local label="$3"

  if [ ! -f "$src" ]; then
    return 0
  fi
  if [ -e "$dst" ]; then
    echo "bootstrap: keeping existing $label at $dst"
    return 0
  fi

  mkdir -p "$(dirname "$dst")"
  sed     -e "s/{{AGENT_NAME}}/$AGENT_NAME/g"     -e "s/{{name}}/$AGENT_NAME/g"     "$src" > "$dst"
  echo "bootstrap: wrote $label from $src to $dst"
}

copy_tree_once() {
  local src="$1"
  local dst="$2"
  local label="$3"

  if [ ! -d "$src" ]; then
    return 0
  fi
  if [ -e "$dst" ]; then
    echo "bootstrap: keeping existing $label at $dst"
    return 0
  fi

  mkdir -p "$(dirname "$dst")"
  cp -a "$src" "$dst"
  find "$dst" -type f \( -name '*.md' -o -name '*.yaml' -o -name '*.yml' -o -name '*.sh' -o -name run \) -print0 |     while IFS= read -r -d '' file; do
      tmp="$file.tmp.$$"
      sed         -e "s/{{AGENT_NAME}}/$AGENT_NAME/g"         -e "s/{{name}}/$AGENT_NAME/g"         "$file" > "$tmp"
      cat "$tmp" > "$file"
      rm -f "$tmp"
    done
  echo "bootstrap: copied $label from $src to $dst"
}

hydrate_chatgpt_oauth_from_env_once() {
  local target="$CONFIG_DIR/chatgpt_oauth.json"

  if [ -f "$target" ]; then
    return 0
  fi
  if [ -z "${TERM_LLM_CHATGPT_OAUTH_JSON_B64:-}" ]; then
    return 0
  fi

  mkdir -p "$CONFIG_DIR"
  umask 077
  if printf '%s' "$TERM_LLM_CHATGPT_OAUTH_JSON_B64" | base64 -d > "$target"; then
    chmod 600 "$target"
    echo "bootstrap: hydrated ChatGPT OAuth credentials from environment"
  else
    rm -f "$target"
    echo "bootstrap: failed to decode TERM_LLM_CHATGPT_OAUTH_JSON_B64" >&2
    return 1
  fi
}

write_default_config_once() {
  local provider="${TERM_LLM_PROVIDER:-}"
  local config_file="$CONFIG_DIR/config.yaml"

  if [ -e "$config_file" ]; then
    echo "bootstrap: keeping existing term-llm config at $config_file"
    return 0
  fi

  mkdir -p "$CONFIG_DIR"
  {
    if [ -n "$provider" ] && [ "$provider" != "skip" ]; then
      echo "default_provider: \"$provider\""
    fi
    if [ "$provider" = "chatgpt" ]; then
      cat <<'CONFIG_YAML'
providers:
  chatgpt:
    model: gpt-5.5-medium
image:
  provider: "chatgpt:gpt-5.4-mini"
CONFIG_YAML
    fi
    if [ "$provider" = "claude-bin" ]; then
      cat <<'CONFIG_YAML'
providers:
  claude-bin:
    model: opus-xhigh
CONFIG_YAML
    fi
    cat <<'CONFIG_YAML'
skills:
  enabled: true
  auto_invoke: true
CONFIG_YAML
  } > "$config_file"
  chmod 600 "$config_file"
  if [ -n "$provider" ] && [ "$provider" != "skip" ]; then
    echo "bootstrap: wrote term-llm config with default_provider=$provider and skills enabled"
  else
    echo "bootstrap: wrote term-llm config with skills enabled"
  fi
}

write_default_volume_init() {
  local init_file="$CONFIG_DIR/init.sh"
  if [ -e "$init_file" ]; then
    echo "bootstrap: keeping existing volume init at $init_file"
    return 0
  fi

  mkdir -p "$CONFIG_DIR"
  cat > "$init_file" <<'VOLUME_INIT'
#!/bin/bash
set -euo pipefail

CONFIG_DIR="/home/agent/.config/term-llm"
SERVICES_DIR="$CONFIG_DIR/services"
SV_DIR="/etc/sv"
RUNSVDIR="/etc/runit/runsvdir"

# Reinstall persisted runit service definitions into the container filesystem.
# /etc is not the source of truth; the Docker volume under $CONFIG_DIR is.
if [ -d "$SERVICES_DIR" ]; then
  mkdir -p "$SV_DIR" "$RUNSVDIR"
  for svc_dir in "$SERVICES_DIR"/*/; do
    [ -d "$svc_dir" ] || continue
    svc_name="$(basename "$svc_dir")"
    target="$SV_DIR/$svc_name"
    rm -rf "$target"
    cp -a "$svc_dir" "$target"
    chmod +x "$target"/run "$target"/finish 2>/dev/null || true
    ln -snf "$target" "$RUNSVDIR/$svc_name"
  done
fi
VOLUME_INIT
  chmod 755 "$init_file"
  echo "bootstrap: wrote default volume init at $init_file"
}

ensure_agent_ownership() {
  mkdir -p "$AGENT_HOME" "$CONFIG_DIR"
  if [ ! -e "$AGENT_HOME/.zshrc" ]; then
    cat > "$AGENT_HOME/.zshrc" <<'ZSHRC'
# term-llm shell niceties
alias tl=term-llm
if ! infocmp "$TERM" >/dev/null 2>&1; then export TERM=xterm-256color; fi
autoload -Uz compinit && compinit
compdef tl=term-llm 2>/dev/null || true
setopt prompt_subst
autoload -Uz colors && colors
bindkey -e
bindkey '^?' backward-delete-char
bindkey '^H' backward-delete-char
PROMPT='%F{cyan}${AGENT_NAME:-agent}%f:%F{yellow}%~%f %# '
ZSHRC
  else
    if ! grep -Eq '^[[:space:]]*alias[[:space:]]+tl=' "$AGENT_HOME/.zshrc"; then
      printf '\n# term-llm convenience alias\nalias tl=term-llm\n' >> "$AGENT_HOME/.zshrc"
    fi
    if ! grep -q 'term-llm shell niceties' "$AGENT_HOME/.zshrc"; then
      cat >> "$AGENT_HOME/.zshrc" <<'ZSHRC'

# term-llm shell niceties
if ! infocmp "$TERM" >/dev/null 2>&1; then export TERM=xterm-256color; fi
setopt prompt_subst
autoload -Uz colors && colors
bindkey -e
bindkey '^?' backward-delete-char
bindkey '^H' backward-delete-char
PROMPT='%F{cyan}${AGENT_NAME:-agent}%f:%F{yellow}%~%f %# '
ZSHRC
    fi
  fi
  chown -R "$AGENT_USER:$AGENT_USER" "$AGENT_HOME"
}

bootstrap_seed_once() {
  if [ -f "$BOOTSTRAP_SENTINEL" ]; then
    return 0
  fi

  echo "bootstrap: first boot for agent=$AGENT_NAME; source=$BOOTSTRAP_DIR"
  mkdir -p "$CONFIG_DIR" "$AGENT_DIR"

  render_file_once "$BOOTSTRAP_DIR/bootstrap.yaml" "$AGENT_DIR/agent.yaml" "agent config"
  render_file_once "$BOOTSTRAP_DIR/system.md" "$AGENT_DIR/system.md" "system prompt"
  render_file_once "$BOOTSTRAP_DIR/soul.md" "$AGENT_DIR/soul.md" "soul"
  copy_tree_once "$BOOTSTRAP_DIR/services" "$CONFIG_DIR/services" "services"
  copy_tree_once "$BOOTSTRAP_DIR/skills" "$CONFIG_DIR/skills" "skills"
  copy_tree_once "$BOOTSTRAP_DIR/memory" "$AGENT_DIR/memory" "memory"
  copy_tree_once "$BOOTSTRAP_DIR/scripts" "$AGENT_DIR/scripts" "agent scripts"

  if [ ! -f "$AGENT_DIR/agent.yaml" ]; then
    echo "Warning: no agent config found after bootstrap."
    echo "  expected: $AGENT_DIR/agent.yaml"
    echo "  bootstrap source: $BOOTSTRAP_DIR/bootstrap.yaml"
  fi

  hydrate_chatgpt_oauth_from_env_once
  write_default_config_once
  write_default_volume_init
  touch "$BOOTSTRAP_SENTINEL"
  echo "bootstrap: complete; future boots ignore /seed and image bootstrap files, then run $CONFIG_DIR/init.sh"
}

bootstrap_seed_once
ensure_agent_ownership

# Volume init hook (runs every container start after first-boot bootstrap).
if [ -f "$CONFIG_DIR/init.sh" ]; then
  bash "$CONFIG_DIR/init.sh"
  ensure_agent_ownership
fi

# No command → boot runit as PID 1
if [ "$#" -eq 0 ]; then
  mkdir -p /etc/runit/runsvdir
  exec runsvdir /etc/runit/runsvdir
fi

exec "$@"
