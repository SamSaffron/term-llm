#!/bin/sh
set -e

REPO_OWNER="samsaffron"
REPO_NAME="term-llm"
BINARY_NAME="term-llm"
DEFAULT_INSTALL_DIR=${TERM_LLM_INSTALL_DIR:-$HOME/.local/bin}

info() {
    printf '==> %s\n' "$1"
}

warn() {
    printf 'Warning: %s\n' "$1" >&2
}

err() {
    printf 'Error: %s\n' "$1" >&2
    exit 1
}

usage() {
    cat <<USAGE
Usage: install.sh [--version <tag>] [--install-dir <path>]

Options:
  --version <tag>       Install a specific release (e.g. v0.3.0 or 0.3.0). Defaults to latest.
  --install-dir <path>  Install to this directory (defaults to ~/.local/bin or TERM_LLM_INSTALL_DIR).
  -h, --help            Show this help message.

You can also set TERM_LLM_INSTALL_DIR to override the default install directory.
When piping with curl, pass flags via: curl ... | sh -s -- --version v0.3.0
USAGE
}

command_exists() {
    command -v "$1" >/dev/null 2>&1
}

expand_path() {
    case "$1" in
        ~*) printf '%s\n' "$HOME${1#~}" ;;
        *) printf '%s\n' "$1" ;;
    esac
}

# HTTP helpers ---------------------------------------------------------------

download_to_stdout() {
    url="$1"
    if command_exists curl; then
        curl -fsSL -H "User-Agent: ${BINARY_NAME}-installer" "$url"
    elif command_exists wget; then
        wget -qO- --header="User-Agent: ${BINARY_NAME}-installer" "$url"
    else
        err "Either curl or wget is required to download files."
    fi
}

download_to_file() {
    url="$1"
    dest="$2"
    if command_exists curl; then
        curl -fsSL -o "$dest" "$url"
    elif command_exists wget; then
        wget -qO "$dest" "$url"
    else
        err "Either curl or wget is required to download files."
    fi
}

# Platform detection --------------------------------------------------------

normalize_os() {
    case "$1" in
        Linux) printf 'linux\n' ;;
        Darwin) printf 'darwin\n' ;;
        *) err "Unsupported operating system: $1" ;;
    esac
}

normalize_arch() {
    case "$1" in
        x86_64|amd64) printf 'amd64\n' ;;
        arm64|aarch64) printf 'arm64\n' ;;
        *) err "Unsupported architecture: $1" ;;
    esac
}

# Release helpers -----------------------------------------------------------

get_latest_version() {
    api_url="https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest"
    tag=$(download_to_stdout "$api_url" | \
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
    [ -n "$tag" ] || err "Unable to determine latest release tag"
    printf '%s\n' "$tag"
}

# Install helpers -----------------------------------------------------------

best_install_dir() {
    if [ -n "$USER_INSTALL_DIR" ]; then
        printf '%s\n' "$USER_INSTALL_DIR"
        return
    fi

    printf '%s\n' "$DEFAULT_INSTALL_DIR"
}

ensure_directory() {
    dir="$1"
    if [ -d "$dir" ]; then
        return
    fi

    if mkdir -p "$dir" 2>/dev/null; then
        return
    fi

    err "Failed to create $dir. Set TERM_LLM_INSTALL_DIR to an accessible path and rerun."
}

install_binary() {
    src="$1"
    dest_dir="$2"
    dest="$dest_dir/$BINARY_NAME"

    if command_exists install; then
        if install -m 755 "$src" "$dest" 2>/dev/null; then
            printf '%s\n' "$dest"
            return 0
        fi
    fi

    if cp "$src" "$dest" 2>/dev/null; then
        chmod 755 "$dest"
        printf '%s\n' "$dest"
        return 0
    fi

    return 1
}

in_path() {
    dir="$1"
    case ":$PATH:" in
        *:"$dir":*) return 0 ;;
        *) return 1 ;;
    esac
}

# Argument parsing ----------------------------------------------------------

USER_VERSION=""
USER_INSTALL_DIR=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -gt 1 ] || err "--version flag requires a value"
            USER_VERSION="$2"
            shift 2
            continue
            ;;
        --install-dir)
            [ "$#" -gt 1 ] || err "--install-dir flag requires a value"
            USER_INSTALL_DIR=$(expand_path "$2")
            shift 2
            continue
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            err "Unknown option: $1"
            ;;
    esac
    shift
done

OS=$(normalize_os "$(uname -s)")
ARCH=$(normalize_arch "$(uname -m)")

if [ -z "$USER_VERSION" ]; then
    info "Fetching latest release information"
    VERSION=$(get_latest_version)
else
    case "$USER_VERSION" in
        v*) VERSION="$USER_VERSION" ;;
        *) VERSION="v$USER_VERSION" ;;
    esac
fi

ASSET_VERSION=${VERSION#v}
ASSET="${REPO_NAME}_${ASSET_VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/${ASSET}"

info "Downloading ${ASSET}"
TMPDIR=$(mktemp -d 2>/dev/null || mktemp -d -t "${REPO_NAME}-install")
trap 'rm -rf "$TMPDIR"' EXIT INT TERM HUP
ARCHIVE_PATH="$TMPDIR/${ASSET}"

download_to_file "$DOWNLOAD_URL" "$ARCHIVE_PATH" || err "Failed to download release asset. Check that version ${VERSION} exists for ${OS}/${ARCH}."

tar -xzf "$ARCHIVE_PATH" -C "$TMPDIR"

BIN_PATH=""
for candidate in "$TMPDIR/$BINARY_NAME" "$TMPDIR/${BINARY_NAME}.exe"; do
    if [ -f "$candidate" ]; then
        BIN_PATH="$candidate"
        break
    fi
done

if [ -z "$BIN_PATH" ]; then
    BIN_PATH=$(find "$TMPDIR" -type f -name "$BINARY_NAME" -o -name "${BINARY_NAME}.exe" 2>/dev/null | head -n 1)
fi

[ -n "$BIN_PATH" ] || err "Failed to locate extracted binary in archive"

INSTALL_DIR=$(expand_path "$(best_install_dir)")
ensure_directory "$INSTALL_DIR"

INSTALLED_PATH=$(install_binary "$BIN_PATH" "$INSTALL_DIR") || err "Installation failed. Set TERM_LLM_INSTALL_DIR to a writeable location and rerun."

info "Installed to $INSTALLED_PATH"

if ! in_path "$INSTALL_DIR"; then
    warn "$INSTALL_DIR is not in your PATH. Add it to your shell profile, e.g. export PATH=\"$INSTALL_DIR:\$PATH\"."
fi

if "$INSTALLED_PATH" version >/dev/null 2>&1; then
    "$INSTALLED_PATH" version
else
    info "Run 'term-llm version' to verify the installation."
fi

info "All done!"
