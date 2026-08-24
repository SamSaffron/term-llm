#!/usr/bin/env bash

set -euo pipefail

if ((BASH_VERSINFO[0] < 4)); then
    echo "this installer requires Bash 4 or newer" >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
user_name=$(id -un)

binary=""
deploy_dir="${HOME}/.config/term-llm/systemd-web"
unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
work_dir=""
host=""
port=""
local_state=""
hub_url=""
hub_node_id=""
hub_node_name=""
hub_token_file=""
hub_action="ask"
start_units=1
interactive=1

binary_set=0
work_dir_set=0
host_set=0
port_set=0
local_state_set=0
env_changed=0
pending_tmp=""
staging_dir=""

cleanup() {
    [[ -z $pending_tmp ]] || rm -f -- "$pending_tmp"
    [[ -z $staging_dir ]] || rm -rf -- "$staging_dir"
}
trap cleanup EXIT

usage() {
    cat <<'EOF'
Install the term-llm Web UI as a systemd user service.

Usage: ./install.sh [options]

Options:
  --binary PATH                 term-llm binary (default: command -v term-llm)
  --deploy-dir PATH             secrets/scripts directory
  --working-directory PATH      serve startup directory (default: $HOME)
  --host HOST                   bind host (default: 127.0.0.1)
  --port PORT                   bind port (default: 8080)
  --local-state                 keep config/data/cache in the deploy directory
  --normal-state                use the account's normal term-llm state
  --hub-url URL                 configure reverse Hub registration
  --hub-node-id ID              unique reverse Hub node ID
  --hub-node-name NAME          optional reverse Hub display name
  --hub-registration-token-file PATH
                                read the Hub registration token from a file
  --disable-hub                 remove reverse Hub registration settings
  --non-interactive             accept new-install defaults or saved choices
  --no-start                    install files without applying or starting units
  -h, --help                    show this help

Resolved installation choices are saved in install.conf. Re-running this script
uses those choices as defaults and preserves secrets unless explicitly changed.
EOF
}

while (($#)); do
    case "$1" in
        --binary)
            binary=${2:?missing value for --binary}
            binary_set=1
            shift 2
            ;;
        --deploy-dir)
            deploy_dir=${2:?missing value for --deploy-dir}
            shift 2
            ;;
        --working-directory)
            work_dir=${2:?missing value for --working-directory}
            work_dir_set=1
            shift 2
            ;;
        --host)
            host=${2:?missing value for --host}
            host_set=1
            shift 2
            ;;
        --port)
            port=${2:?missing value for --port}
            port_set=1
            shift 2
            ;;
        --local-state)
            local_state=yes
            local_state_set=1
            shift
            ;;
        --normal-state)
            local_state=no
            local_state_set=1
            shift
            ;;
        --hub-url)
            hub_url=${2:?missing value for --hub-url}
            hub_action=configure
            shift 2
            ;;
        --hub-node-id)
            hub_node_id=${2:?missing value for --hub-node-id}
            hub_action=configure
            shift 2
            ;;
        --hub-node-name)
            hub_node_name=${2:?missing value for --hub-node-name}
            hub_action=configure
            shift 2
            ;;
        --hub-registration-token-file)
            hub_token_file=${2:?missing value for --hub-registration-token-file}
            hub_action=configure
            shift 2
            ;;
        --disable-hub)
            hub_action=disable
            shift
            ;;
        --non-interactive)
            interactive=0
            shift
            ;;
        --no-start)
            start_units=0
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

heading() {
    printf '\n\033[1;36m==> %s\033[0m\n' "$1"
}

note() {
    printf '    %s\n' "$1"
}

ask_value() {
    local prompt=$1 default=$2 answer
    if ! read -r -p "$prompt [$default]: " answer; then
        echo "aborted" >&2
        exit 130
    fi
    printf '%s' "${answer:-$default}"
}

ask_yes_no() {
    local prompt=$1 default=$2 answer suffix
    if [[ $default == yes ]]; then suffix='Y/n'; else suffix='y/N'; fi
    if ! read -r -p "$prompt [$suffix]: " answer; then
        echo "aborted" >&2
        exit 130
    fi
    answer=${answer:-$default}
    case "${answer,,}" in
        y|yes) return 0 ;;
        *) return 1 ;;
    esac
}

ask_secret() {
    local prompt=$1 answer
    if ! read -r -s -p "$prompt: " answer; then
        printf '\naborted\n' >&2
        exit 130
    fi
    printf '\n' >&2
    printf '%s' "$answer"
}

reject_newline() {
    local name=$1 value=$2
    if [[ $value == *$'\n'* || $value == *$'\r'* ]]; then
        echo "$name must not contain a newline" >&2
        exit 2
    fi
}

validate_unit_path() {
    local name=$1 value=$2
    if [[ ! $value =~ ^/[A-Za-z0-9._/+@:-]+$ ]]; then
        echo "$name must be an absolute path containing only A-Z, a-z, 0-9, ., _, /, +, @, :, or -: $value" >&2
        exit 2
    fi
}

env_quote() {
    local value=$1
    value=${value//\\/\\\\}
    value=${value//\"/\\\"}
    printf '"%s"' "$value"
}

set_env() {
    local key=$1 value=$2 mode
    reject_newline "$key" "$value"
    [[ -f ${deploy_dir}/.env ]] || { echo "missing environment file: ${deploy_dir}/.env" >&2; exit 1; }
    mode=$(stat -c %a "${deploy_dir}/.env")
    pending_tmp=$(mktemp "${deploy_dir}/.env.tmp.XXXXXX")
    if ! grep -v "^${key}=" "${deploy_dir}/.env" >"$pending_tmp"; then
        [[ -r ${deploy_dir}/.env ]] || { echo "cannot read ${deploy_dir}/.env" >&2; exit 1; }
    fi
    printf '%s=%s\n' "$key" "$(env_quote "$value")" >>"$pending_tmp"
    chmod "$mode" "$pending_tmp"
    mv -f -- "$pending_tmp" "${deploy_dir}/.env"
    pending_tmp=""
    env_changed=1
}

unset_env() {
    local key=$1 mode
    [[ -f ${deploy_dir}/.env ]] || return 0
    grep -q "^${key}=" "${deploy_dir}/.env" || return 0
    mode=$(stat -c %a "${deploy_dir}/.env")
    pending_tmp=$(mktemp "${deploy_dir}/.env.tmp.XXXXXX")
    grep -v "^${key}=" "${deploy_dir}/.env" >"$pending_tmp" || true
    chmod "$mode" "$pending_tmp"
    mv -f -- "$pending_tmp" "${deploy_dir}/.env"
    pending_tmp=""
    env_changed=1
}

generate_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
    fi
}

load_state() {
    local key value state_version="" state_file=${deploy_dir}/install.conf
    [[ -f $state_file ]] || return 0
    while IFS='=' read -r key value; do
        case "$key" in
            STATE_VERSION) state_version=$value ;;
            BINARY) ((binary_set)) || binary=$value ;;
            WORK_DIR) ((work_dir_set)) || work_dir=$value ;;
            HOST) ((host_set)) || host=$value ;;
            PORT) ((port_set)) || port=$value ;;
            LOCAL_STATE) ((local_state_set)) || local_state=$value ;;
            HUB_ENABLED) [[ $hub_action != ask ]] || hub_action=$([[ $value == yes ]] && echo preserve || echo ask) ;;
            HUB_URL) [[ -n $hub_url ]] || hub_url=$value ;;
            HUB_NODE_ID) [[ -n $hub_node_id ]] || hub_node_id=$value ;;
            HUB_NODE_NAME) [[ -n $hub_node_name ]] || hub_node_name=$value ;;
        esac
    done <"$state_file"
    if [[ $state_version != 1 ]]; then
        echo "unsupported installer state version in $state_file: ${state_version:-missing}" >&2
        exit 2
    fi
}

atomic_install() {
    local source=$1 destination=$2 mode=$3
    if [[ -f $destination ]] && cmp -s "$source" "$destination"; then
        return 1
    fi
    pending_tmp=$(mktemp "${destination}.tmp.XXXXXX")
    install -m "$mode" "$source" "$pending_tmp"
    mv -f -- "$pending_tmp" "$destination"
    pending_tmp=""
    return 0
}

if [[ $interactive -eq 1 && ! -t 0 ]]; then interactive=0; fi

validate_unit_path "deployment directory" "$deploy_dir"
if [[ $deploy_dir == / || $deploy_dir == "$HOME" ]]; then
    echo "refusing unsafe deployment directory: $deploy_dir" >&2
    exit 2
fi
load_state

binary=${binary:-$(command -v term-llm || true)}
work_dir=${work_dir:-$HOME}
host=${host:-127.0.0.1}
port=${port:-8080}
local_state=${local_state:-no}

heading "Planning the deployment"
if [[ -z $binary ]]; then
    echo "term-llm was not found on PATH; rerun with --binary PATH" >&2
    exit 1
fi
if [[ $interactive -eq 1 ]]; then
    binary=$(ask_value "term-llm binary" "$binary")
    work_dir=$(ask_value "Workspace term-llm should start in" "$work_dir")
    host=$(ask_value "Bind host" "$host")
    port=$(ask_value "Bind port" "$port")
    if [[ $local_state == no ]]; then existing_state_default=yes; else existing_state_default=no; fi
    if ask_yes_no "Use your existing term-llm configuration and sessions?" "$existing_state_default"; then
        local_state=no
    else
        local_state=yes
    fi
fi
if [[ $binary != /* ]]; then
    binary_dir=$(CDPATH= cd -- "$(dirname -- "$binary")" && pwd)
    binary="${binary_dir}/$(basename -- "$binary")"
fi

validate_unit_path "term-llm binary" "$binary"
validate_unit_path "systemd user unit directory" "$unit_dir"
validate_unit_path "working directory" "$work_dir"
[[ -x $binary ]] || { echo "term-llm binary is not executable: $binary" >&2; exit 1; }
[[ -d $work_dir ]] || { echo "working directory does not exist: $work_dir" >&2; exit 1; }
if [[ ! $port =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
    echo "port must be between 1 and 65535" >&2
    exit 2
fi
if [[ ! $host =~ ^[A-Za-z0-9._:-]+$ ]]; then
    echo "host contains unsupported characters: $host" >&2
    exit 2
fi
if [[ $local_state != yes && $local_state != no ]]; then
    echo "invalid saved LOCAL_STATE value: $local_state" >&2
    exit 2
fi

note "Binary:    $binary"
note "Workspace: $work_dir"
note "Listener:  $host:$port"
note "Files:     $deploy_dir"

heading "Installing private configuration"
if [[ -e $deploy_dir && ! -d $deploy_dir ]]; then
    echo "deployment path exists but is not a directory: $deploy_dir" >&2
    exit 2
fi
if [[ ! -d $deploy_dir ]]; then
    install -d -m 700 "$deploy_dir"
fi
mkdir -p "$unit_dir"
scripts_changed=0
if atomic_install "${script_dir}/restart-if-binary-changed.sh" "${deploy_dir}/restart-if-binary-changed.sh" 755; then scripts_changed=1; fi
if atomic_install "${script_dir}/run-term-llm-web.sh" "${deploy_dir}/run-term-llm-web.sh" 755; then scripts_changed=1; fi
if [[ ! -e ${deploy_dir}/.env ]]; then
    install -m 600 "${script_dir}/.env.example" "${deploy_dir}/.env"
fi
if ! grep -Eq '^TERM_LLM_SERVE_TOKEN=("[^"]+"|[^[:space:]]+)$' "${deploy_dir}/.env" || grep -q 'replace-with-a-long-random-token' "${deploy_dir}/.env"; then
    set_env TERM_LLM_SERVE_TOKEN "$(generate_token)"
    note "Generated a stable Web UI bearer token."
else
    note "Preserved the existing Web UI bearer token."
fi

if [[ $interactive -eq 1 ]] && ask_yes_no "Configure provider API keys for this service?" no; then
    provider_vars=(
        ANTHROPIC_API_KEY
        OPENAI_API_KEY
        GEMINI_API_KEY
        OPENROUTER_API_KEY
        ZEN_API_KEY
        OPENCODE_API_KEY
        XAI_API_KEY
        VENICE_API_KEY
        NEARAI_API_KEY
        SAMBANOVA_API_KEY
        VLLM_API_KEY
        CURSOR_API_KEY
    )
    for provider_var in "${provider_vars[@]}"; do
        if grep -q "^${provider_var}=" "${deploy_dir}/.env"; then
            provider_key=$(ask_secret "$provider_var (leave blank to keep existing)")
        else
            provider_key=$(ask_secret "$provider_var (leave blank to skip)")
        fi
        if [[ -n $provider_key ]]; then
            set_env "$provider_var" "$provider_key"
        fi
        unset provider_key
    done
fi

if [[ $hub_action == ask && $interactive -eq 1 ]]; then
    if ask_yes_no "Register this service as a reverse-connected Hub node?" no; then hub_action=configure; else hub_action=preserve; fi
elif [[ $hub_action == preserve && $interactive -eq 1 ]]; then
    if ask_yes_no "Update the existing reverse Hub registration settings?" no; then hub_action=configure; fi
fi

if [[ $hub_action == configure ]]; then
    default_node=${HOSTNAME:-}
    default_node=${default_node%%.*}
    if [[ -z $default_node ]]; then default_node=$(uname -n 2>/dev/null || printf node); default_node=${default_node%%.*}; fi
    if [[ $interactive -eq 1 ]]; then
        hub_url=$(ask_value "Hub URL" "${hub_url:-https://hub.example.com}")
        hub_node_id=$(ask_value "Unique Hub node ID" "${hub_node_id:-$default_node}")
        hub_node_name=$(ask_value "Hub node display name" "${hub_node_name:-$hub_node_id}")
    fi
    if [[ $hub_url != *://* ]]; then
        hub_url="https://${hub_url}"
    fi
    [[ $hub_url =~ ^https?://[^[:space:]]+$ ]] || { echo "Hub URL must use http:// or https:// and contain no whitespace" >&2; exit 2; }
    [[ $hub_node_id =~ ^[A-Za-z0-9._-]+$ ]] || { echo "Hub node ID may contain only letters, numbers, ., _, and -" >&2; exit 2; }
    reject_newline "Hub node name" "$hub_node_name"

    registration_token=""
    preserve_token=0
    if [[ -n $hub_token_file ]]; then
        [[ -r $hub_token_file ]] || { echo "cannot read Hub registration token file: $hub_token_file" >&2; exit 2; }
        IFS= read -r registration_token <"$hub_token_file" || true
    elif [[ $interactive -eq 1 ]]; then
        if grep -q '^TERM_LLM_HUB_REGISTRATION_TOKEN=' "${deploy_dir}/.env" && ! ask_yes_no "Replace the saved Hub registration token?" no; then
            preserve_token=1
        else
            registration_token=$(ask_secret "Hub registration token")
        fi
    fi
    if ((preserve_token == 0)); then
        [[ -n $registration_token ]] || { echo "Hub registration requires --hub-registration-token-file in non-interactive mode" >&2; exit 2; }
        set_env TERM_LLM_HUB_REGISTRATION_TOKEN "$registration_token"
    fi
    set_env TERM_LLM_SERVE_HUB_URL "$hub_url"
    set_env TERM_LLM_SERVE_HUB_NODE_ID "$hub_node_id"
    set_env TERM_LLM_SERVE_HUB_NODE_NAME "$hub_node_name"
    set_env TERM_LLM_SERVE_HUB_REGISTER "1"
    hub_enabled=yes
    unset registration_token
    note "Configured reverse Hub registration for node $hub_node_id."
elif [[ $hub_action == disable ]]; then
    unset_env TERM_LLM_SERVE_HUB_URL
    unset_env TERM_LLM_SERVE_HUB_NODE_ID
    unset_env TERM_LLM_SERVE_HUB_NODE_NAME
    unset_env TERM_LLM_SERVE_HUB_REGISTER
    unset_env TERM_LLM_HUB_REGISTRATION_TOKEN
    hub_enabled=no
    hub_url=""; hub_node_id=""; hub_node_name=""
    note "Removed reverse Hub registration settings."
else
    if grep -q '^TERM_LLM_SERVE_HUB_REGISTER=' "${deploy_dir}/.env"; then hub_enabled=yes; else hub_enabled=no; fi
fi

heading "Generating and validating systemd user units"
staging_dir=$(mktemp -d)
xdg_block=""
if [[ $local_state == yes ]]; then
    xdg_block="Environment=XDG_CONFIG_HOME=${deploy_dir}/config
Environment=XDG_DATA_HOME=${deploy_dir}/data
Environment=XDG_CACHE_HOME=${deploy_dir}/cache"
    install -d -m 700 "${deploy_dir}/config" "${deploy_dir}/data" "${deploy_dir}/cache"
fi
cat >"${staging_dir}/term-llm-web.service" <<EOF
[Unit]
Description=term-llm Web UI
Documentation=https://term-llm.com/guides/web-ui-and-api/
StartLimitIntervalSec=60
StartLimitBurst=3

[Service]
Type=exec
WorkingDirectory=${work_dir}
ExecStart=${deploy_dir}/run-term-llm-web.sh ${binary} ${host} ${port}
# Required on purpose: a missing file must not cause a fresh random login token.
EnvironmentFile=${deploy_dir}/.env
${xdg_block}
Restart=on-failure
RestartPreventExitStatus=78
RestartSec=5
UMask=0077

[Install]
WantedBy=default.target
EOF
cat >"${staging_dir}/term-llm-web-update-check.service" <<EOF
[Unit]
Description=Restart term-llm Web UI if its binary changed
Documentation=https://term-llm.com/guides/web-ui-and-api/

[Service]
Type=oneshot
TimeoutStartSec=2min
ExecStart=${deploy_dir}/restart-if-binary-changed.sh term-llm-web.service ${binary}
EOF
cp "${script_dir}/term-llm-web-update-check.timer" "${staging_dir}/term-llm-web-update-check.timer"

if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze --user verify "${staging_dir}"/*.service "${staging_dir}"/*.timer
else
    note "systemd-analyze is unavailable; skipped unit validation."
fi

service_changed=0
checker_changed=0
timer_changed=0
if atomic_install "${staging_dir}/term-llm-web.service" "${unit_dir}/term-llm-web.service" 644; then service_changed=1; fi
if atomic_install "${staging_dir}/term-llm-web-update-check.service" "${unit_dir}/term-llm-web-update-check.service" 644; then checker_changed=1; fi
if atomic_install "${staging_dir}/term-llm-web-update-check.timer" "${unit_dir}/term-llm-web-update-check.timer" 644; then timer_changed=1; fi

pending_tmp=$(mktemp "${deploy_dir}/install.conf.tmp.XXXXXX")
cat >"$pending_tmp" <<EOF
STATE_VERSION=1
BINARY=${binary}
WORK_DIR=${work_dir}
HOST=${host}
PORT=${port}
LOCAL_STATE=${local_state}
HUB_ENABLED=${hub_enabled}
HUB_URL=${hub_url}
HUB_NODE_ID=${hub_node_id}
HUB_NODE_NAME=${hub_node_name}
EOF
chmod 600 "$pending_tmp"
mv -f -- "$pending_tmp" "${deploy_dir}/install.conf"
pending_tmp=""

if [[ $start_units -eq 1 ]]; then
    heading "Applying systemd configuration"
    systemctl --user daemon-reload
    systemctl --user enable term-llm-web.service term-llm-web-update-check.timer
    if ((service_changed || env_changed || scripts_changed)); then
        if systemctl --user is-active --quiet term-llm-web.service; then
            systemctl --user restart term-llm-web.service
            note "Restarted the Web UI to apply changed settings."
        else
            systemctl --user start term-llm-web.service
        fi
    elif ! systemctl --user is-active --quiet term-llm-web.service; then
        systemctl --user start term-llm-web.service
    fi
    if ((timer_changed)); then
        systemctl --user restart term-llm-web-update-check.timer
    elif ! systemctl --user is-active --quiet term-llm-web-update-check.timer; then
        systemctl --user start term-llm-web-update-check.timer
    fi
    note "Web UI: http://${host}:${port}/ui/"

    if [[ $interactive -eq 1 ]] && command -v loginctl >/dev/null 2>&1; then
        linger=$(loginctl show-user "$user_name" -p Linger --value 2>/dev/null || true)
        if [[ $linger != yes ]] && ask_yes_no "Keep this user service running after logout (enable linger with sudo)?" yes; then
            sudo loginctl enable-linger "$user_name"
        fi
    fi
else
    heading "Installed without starting"
    note "Run: systemctl --user daemon-reload"
    note "Run: systemctl --user enable --now term-llm-web.service term-llm-web-update-check.timer"
fi

heading "Installed — useful maintenance commands"
cat <<EOF
    Follow logs:       journalctl --user -u term-llm-web.service -f
    Check status:      systemctl --user status term-llm-web.service
    Restart now:       systemctl --user restart term-llm-web.service
    Check for update:  systemctl --user start term-llm-web-update-check.service
    See next 04:00:    systemctl --user list-timers term-llm-web-update-check.timer
    Show login token:  sed -n 's/^TERM_LLM_SERVE_TOKEN="\(.*\)"$/\1/p' ${deploy_dir}/.env
    Edit secrets:      \${EDITOR:-vi} ${deploy_dir}/.env
    Re-run installer:  ${script_dir}/install.sh

The 04:00 timer never downloads updates. After your normal updater replaces
${binary}, it compares that file with the running executable and restarts only
when they differ.
EOF
if [[ $local_state == yes ]]; then
    cat <<EOF

This deployment uses isolated term-llm state. Edit its config with:
    XDG_CONFIG_HOME=${deploy_dir}/config term-llm config edit
EOF
fi
