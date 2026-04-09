#!/bin/bash
# Boot hook — runs every container start via entrypoint.
# On first boot: installs runit services from /seed/services/ and generates
# a volume-side bootstrap-services.sh so the agent owns service setup on
# every subsequent boot/rebuild, independent of the seed bind-mount.

SEED_SERVICES="/seed/services"
SV_DIR="/etc/sv"
RUNSVDIR="/etc/runit/runsvdir"
SENTINEL="/root/.config/term-llm/.services-seeded"

if [ -f "$SENTINEL" ]; then
  exit 0
fi

if [ ! -d "$SEED_SERVICES" ]; then
  exit 0
fi

mkdir -p "$SV_DIR" "$RUNSVDIR"

for svc_dir in "$SEED_SERVICES"/*/; do
  svc_name="$(basename "$svc_dir")"
  target="$SV_DIR/$svc_name"

  rm -rf "$target"
  cp -r "$svc_dir" "$target"
  chmod +x "$target"/run "$target"/finish 2>/dev/null || true

  ln -sf "$target" "$RUNSVDIR/$svc_name"
done

# Generate a volume-side bootstrap script that embeds each service's run script.
# This lets the agent recreate ephemeral /etc/sv/ and /etc/runit/runsvdir/
# on every container rebuild without needing the seed bind-mount.
AGENT_NAME="${AGENT_NAME:-agent}"
AGENT_SCRIPTS="/root/.config/term-llm/agents/$AGENT_NAME/scripts"
BOOTSTRAP="$AGENT_SCRIPTS/bootstrap-services.sh"

if [ ! -f "$BOOTSTRAP" ]; then
  mkdir -p "$AGENT_SCRIPTS"

  {
    cat << 'SCRIPT_HEADER'
#!/bin/bash
set -euo pipefail
RUNSVDIR="/etc/runit/runsvdir"
SVDIR="/etc/sv"
mkdir -p "$RUNSVDIR" "$SVDIR"
SCRIPT_HEADER

    for svc_dir in "$SEED_SERVICES"/*/; do
      svc_name="$(basename "$svc_dir")"
      run_src="$SV_DIR/$svc_name/run"
      [ -f "$run_src" ] || continue
      encoded=$(base64 -w 0 < "$run_src")
      printf '\n# service: %s\n' "$svc_name"
      printf 'mkdir -p "$SVDIR/%s"\n' "$svc_name"
      printf 'base64 -d << '"'"'%s'"'"' > "$SVDIR/%s/run"\n' "EOF_$svc_name" "$svc_name"
      printf '%s\n' "$encoded"
      printf '%s\n' "EOF_$svc_name"
      printf 'chmod 755 "$SVDIR/%s/run"\n' "$svc_name"
      printf 'rm -f "$SVDIR/%s/down"\n' "$svc_name"
      printf 'ln -snf "$SVDIR/%s" "$RUNSVDIR/%s"\n' "$svc_name" "$svc_name"
    done

    cat << 'SCRIPT_FOOTER'

if command -v sv >/dev/null 2>&1; then
  for _svc in "$RUNSVDIR"/*/; do
    _status="$(sv status "$_svc" 2>&1 || true)"
    case "$_status" in
      run:*) ;;
      *) sv start "$_svc" >/dev/null 2>&1 || true ;;
    esac
  done
fi
SCRIPT_FOOTER
  } > "$BOOTSTRAP"
  chmod 755 "$BOOTSTRAP"
  echo "init: bootstrap-services.sh written to $BOOTSTRAP"
fi

# Write the volume init.sh to call bootstrap on every boot (if not already present).
# Guards against overwriting a custom init.sh the agent may already have.
VOLUME_INIT="/root/.config/term-llm/init.sh"
if [ ! -f "$VOLUME_INIT" ]; then
  cat > "$VOLUME_INIT" << VOLINIT
#!/bin/bash
set -euo pipefail
bash "$BOOTSTRAP"
VOLINIT
  chmod 755 "$VOLUME_INIT"
  echo "init: volume init.sh written"
fi

mkdir -p "$(dirname "$SENTINEL")"
touch "$SENTINEL"
echo "init: services seeded"
