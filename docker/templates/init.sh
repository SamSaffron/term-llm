#!/bin/bash
# Boot hook — runs every container start via entrypoint.
# Installs runit services from /seed/services/ on first boot only.

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

mkdir -p "$(dirname "$SENTINEL")"
touch "$SENTINEL"
echo "init: services seeded"
