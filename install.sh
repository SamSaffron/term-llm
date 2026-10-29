#!/bin/sh
set -e

# Stable, ASCII-only validation and tar listings on macOS and Linux.
LC_ALL=C
export LC_ALL

ARCHIVE_LIMIT=134217728       # 128 MiB
CHECKSUM_LIMIT=1048576        # 1 MiB
BINARY_LIMIT=268435456        # 256 MiB
TAR_LIMIT=536870912           # 512 MiB decompressed

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

Maximum sizes: archive 128 MiB, checksums 1 MiB, binary 256 MiB,
decompressed archive 512 MiB. Portable file caps are conservative: on shells
with 512-byte ulimit blocks (including typical curl | sh installs), effective
limits are archive 64 MiB, checksums 512 KiB, binary 128 MiB, and decompressed
archive 256 MiB.
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

# curl is required: wget variants do not uniformly enforce HTTPS-only redirects.
# POSIX shells use 512-byte ulimit blocks, some shells use 1024. Using 1024
# conservatively caps writes at or below the limit on both; some shells thus
# reject files at half the advertised maximum. Always check the final size too.
bounded_output() (
    limit="$1"
    dest="$2"
    shift 2
    # macOS/Linux shells support -c; avoid core dumps on file-limit failures.
    # shellcheck disable=SC3045
    ulimit -c 0 || exit 1
    ulimit -f "$((limit / 1024))" || exit 1
    "$@" > "$dest" || exit 1
    [ "$(wc -c < "$dest")" -le "$limit" ]
)

download_to_file() (
    url="$1"
    dest="$2"
    limit="$3"
    shift 3
    # Also cap disk writes for curl versions that only check Content-Length.
    # macOS/Linux shells support -c; avoid core dumps on file-limit failures.
    # shellcheck disable=SC3045
    ulimit -c 0 || exit 1
    ulimit -f "$((limit / 1024))" || exit 1
    # Ignore curlrc: local defaults must not add URLs or weaken transport flags.
    curl -q -fsSL --proto '=https' --proto-redir '=https' \
        --max-redirs 5 --connect-timeout 20 --max-time 300 \
        --max-filesize "$limit" -o "$dest" "$@" "$url" || exit 1
    [ "$(wc -c < "$dest")" -le "$limit" ]
)

validate_version() {
    case "$1" in
        v[0-9]*) ;;
        *) err "Invalid release version: expected v followed by a digit" ;;
    esac
    case "$1" in
        *[!a-zA-Z0-9._+-]*) err "Invalid release version: unsafe tag characters" ;;
    esac
}

# Checksums provide integrity, NOT publisher authentication: both files come
# from the same release. There is no release signature available to verify.
read_checksum() {
    expected=$(awk -v asset="$ASSET" '
        { sub(/\r$/, "") }
        $NF == asset || $NF == "*" asset {
            count++
            digest = substr($0, 1, 64)
            if (length($0) != 66 + length(asset) ||
                digest !~ /^[0-9a-fA-F]+$/ || length(digest) != 64 ||
                substr($0, 65, 1) != " " ||
                (substr($0, 66, 1) != " " && substr($0, 66, 1) != "*") ||
                substr($0, 67) != asset) bad = 1
        }
        END {
            if (count != 1 || bad) exit 1
            print tolower(digest)
        }
    ' "$CHECKSUM_PATH") || err "Missing, malformed, or duplicate checksum for $ASSET"
}

verify_checksum() {
    if command_exists sha256sum; then
        actual=$(sha256sum "$ARCHIVE_PATH") || err "SHA256 calculation failed"
    else
        actual=$(shasum -a 256 "$ARCHIVE_PATH") || err "SHA256 calculation failed"
    fi
    actual=${actual%% *}
    [ "$actual" = "$expected" ] || err "SHA256 checksum mismatch for $ASSET"
}

extract_binary() {
    # Do not let environment-supplied tar options change member selection.
    unset TAR_OPTIONS
    tar_path="$TMPDIR/archive.tar"
    bounded_output "$TAR_LIMIT" "$tar_path" gzip -dc "$ARCHIVE_PATH" ||
        err "Invalid or oversized decompressed archive (see --help for shell-specific limits)"

    # Never extract archive paths into the filesystem. Validate names, require
    # one exact top-level binary, and stream ONLY that member into a fixed file.
    # Listing output is bounded as well (many tiny entries can expand it).
    bounded_output "$TAR_LIMIT" "$TMPDIR/members" tar -tf "$tar_path" ||
        err "Unable to list archive"
    awk -v binary="$BINARY_NAME" '
        /[^a-zA-Z0-9._+\/-]/ || /^\// || /(^|\/)\.\.?(\/|$)/ { bad = 1 }
        $0 == binary { count++ }
        END { if (bad || count != 1) exit 1 }
    ' "$TMPDIR/members" || err "Unsafe archive paths or missing/duplicate top-level binary"

    bounded_output "$CHECKSUM_LIMIT" "$TMPDIR/member-type" \
        tar -tvf "$tar_path" "$BINARY_NAME" || err "Unable to inspect binary member"
    awk 'substr($0, 1, 1) != "-" { bad = 1 }
         END { if (bad || NR != 1) exit 1 }' "$TMPDIR/member-type" ||
        err "Archive binary must be a single regular file, not a link"

    BIN_PATH="$TMPDIR/$BINARY_NAME"
    bounded_output "$BINARY_LIMIT" "$BIN_PATH" \
        tar -xOf "$tar_path" "$BINARY_NAME" || err "Invalid or oversized binary (see --help for shell-specific limits)"
    [ -s "$BIN_PATH" ] || err "Archive binary is empty"
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
    latest_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest"

    # Only the effective URL is needed. HEAD avoids downloading GitHub's HTML;
    # ignore its hypothetical body length while still bounding actual headers.
    release_url=$(download_to_file "$latest_url" "$TMPDIR/latest.headers" "$CHECKSUM_LIMIT" \
        --head --ignore-content-length \
        -w '%{url_effective}\n' -H "User-Agent: ${BINARY_NAME}-installer") ||
        err "Unable to resolve latest release over HTTPS"

    case "$release_url" in
        "https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/tag/"*)
            tag=${release_url#"https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/tag/"} ;;
        *) err "Unable to determine latest release tag" ;;
    esac

    validate_version "$tag"
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
            [ -n "$2" ] || err "--version flag requires a nonempty value"
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

command_exists curl || err "curl is required for HTTPS-only downloads and redirects (wget is not supported)."
command_exists tar || err "tar is required to unpack release archives."
command_exists gzip || err "gzip is required to decompress release archives."
command_exists awk || err "awk is required to validate checksums and archive members."
command_exists sha256sum || command_exists shasum || err "sha256sum or shasum is required to verify release integrity."

OS=$(normalize_os "$(uname -s)")
ARCH=$(normalize_arch "$(uname -m)")

if [ -n "$USER_VERSION" ]; then
    case "$USER_VERSION" in
        v*) VERSION="$USER_VERSION" ;;
        *) VERSION="v$USER_VERSION" ;;
    esac
    validate_version "$VERSION"
fi

TMPDIR=$(mktemp -d 2>/dev/null || mktemp -d -t "${REPO_NAME}-install")
trap 'rm -rf "$TMPDIR"' EXIT
trap 'exit 1' INT TERM HUP

if [ -z "$USER_VERSION" ]; then
    info "Fetching latest release information"
    VERSION=$(get_latest_version)
fi

ASSET_VERSION=${VERSION#v}
ASSET="${REPO_NAME}_${ASSET_VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/${ASSET}"
CHECKSUM_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/checksums.txt"
ARCHIVE_PATH="$TMPDIR/${ASSET}"
CHECKSUM_PATH="$TMPDIR/checksums.txt"

info "Downloading ${ASSET} and checksums.txt"
download_to_file "$CHECKSUM_URL" "$CHECKSUM_PATH" "$CHECKSUM_LIMIT" || err "Failed to download release checksums (maximum 1 MiB)."
read_checksum
download_to_file "$DOWNLOAD_URL" "$ARCHIVE_PATH" "$ARCHIVE_LIMIT" || err "Failed to download release asset (maximum 128 MiB). Check version ${VERSION} for ${OS}/${ARCH}."
verify_checksum
extract_binary

INSTALL_DIR=$(expand_path "$(best_install_dir)")
ensure_directory "$INSTALL_DIR"

INSTALLED_PATH=$(install_binary "$BIN_PATH" "$INSTALL_DIR") || err "Installation failed. Set TERM_LLM_INSTALL_DIR to a writeable location and rerun."

info "Installed to $INSTALLED_PATH"

if ! in_path "$INSTALL_DIR"; then
    warn "$INSTALL_DIR is not in your PATH. Add it to your shell profile, e.g. export PATH=\"$INSTALL_DIR:\$PATH\"."
fi

info "Run 'term-llm version' to verify the installation."
info "All done!"
