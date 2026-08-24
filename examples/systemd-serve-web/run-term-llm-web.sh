#!/bin/sh

set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: $0 BINARY HOST PORT" >&2
    exit 2
fi

binary=$1
host=$2
port=$3

set -- "$binary" serve web --host "$host" --port "$port"

hub_url=${TERM_LLM_SERVE_HUB_URL:-}
hub_node_id=${TERM_LLM_SERVE_HUB_NODE_ID:-}
hub_node_name=${TERM_LLM_SERVE_HUB_NODE_NAME:-}
hub_register=${TERM_LLM_SERVE_HUB_REGISTER:-}

case "$hub_register" in
    0|false|no|'') exec "$@" ;;
    1|true|yes) ;;
    *)
        echo "TERM_LLM_SERVE_HUB_REGISTER must be 1, true, yes, 0, false, or no" >&2
        exit 78
        ;;
esac

if [ -z "$hub_url" ] || [ -z "$hub_node_id" ]; then
    echo "reverse Hub configuration requires TERM_LLM_SERVE_HUB_URL and TERM_LLM_SERVE_HUB_NODE_ID" >&2
    exit 78
fi
if [ -z "${TERM_LLM_HUB_REGISTRATION_TOKEN:-}" ]; then
    echo "reverse Hub registration requires TERM_LLM_HUB_REGISTRATION_TOKEN" >&2
    exit 78
fi

set -- "$@" \
    --hub-url "$hub_url" \
    --hub-node-id "$hub_node_id" \
    --hub-connect reverse \
    --hub-register
if [ -n "$hub_node_name" ]; then
    set -- "$@" --hub-node-name "$hub_node_name"
fi

exec "$@"
