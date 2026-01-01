#!/bin/bash

# Release script for term-llm
# Usage:
#   ./scripts/release.sh v1.0.0
#   ./scripts/release.sh --auto

set -euo pipefail

usage() {
    cat <<USAGE
Usage: $0 [--auto | <version>]

Examples:
  $0 v1.0.0        # release explicit version
  $0 --auto        # bump patch version based on latest GitHub release
USAGE
}

require_clean_worktree() {
    if [ -n "$(git status --porcelain)" ]; then
        echo "Error: Working directory is not clean. Please commit or stash changes." >&2
        exit 1
    fi
}

fetch_latest_release_tag() {
    # Fetch tags from remote to ensure we have the latest
    if git remote | grep -qx "origin"; then
        git fetch --quiet --tags origin || true
    else
        git fetch --quiet --tags || true
    fi

    # Get the latest version tag
    local tag
    tag=$(git tag -l "v*" --sort=v:refname | tail -n1)
    if [ -n "$tag" ]; then
        printf '%s\n' "$tag"
    fi
}

compute_next_patch_version() {
    local latest raw major minor patch
    latest=$(fetch_latest_release_tag)
    if [ -z "$latest" ]; then
        latest="v0.0.0"
    fi
    raw=${latest#v}
    if [[ ! $raw =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "Error: Latest release tag '$latest' is not in vMAJOR.MINOR.PATCH format." >&2
        exit 1
    fi
    IFS='.' read -r major minor patch <<<"$raw"
    patch=$((patch + 1))
    printf 'v%d.%d.%d\n' "$major" "$minor" "$patch"
}

AUTO=false
VERSION=""

while [ $# -gt 0 ]; do
    case "$1" in
        --auto)
            AUTO=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        --*)
            echo "Unknown option: $1" >&2
            usage
            exit 1
            ;;
        *)
            if [ -n "$VERSION" ]; then
                echo "Error: Multiple version arguments provided." >&2
                usage
                exit 1
            fi
            VERSION="$1"
            shift
            ;;
    esac
done

if [ "$AUTO" = "true" ] && [ -n "$VERSION" ]; then
    echo "Error: Cannot use --auto and an explicit version together." >&2
    usage
    exit 1
fi

if [ "$AUTO" != "true" ] && [ -z "$VERSION" ]; then
    usage
    exit 1
fi

if [ "$AUTO" = "true" ]; then
    VERSION=$(compute_next_patch_version)
    echo "Auto-detected next version: $VERSION"
fi

if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format v1.0.0" >&2
    exit 1
fi

echo "Creating release $VERSION..."

# Ensure we're on main branch
CURRENT_BRANCH=$(git branch --show-current)
if [ "$CURRENT_BRANCH" != "main" ]; then
    echo "Warning: You're not on the main branch (currently on $CURRENT_BRANCH)"
    read -p "Continue anyway? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

require_clean_worktree

# Create and push tag
echo "Creating tag $VERSION..."
git tag -a "$VERSION" -m "Release $VERSION"
git push origin "$VERSION"

echo "Release $VERSION created successfully!"
echo "GitHub Actions will now build and publish the release automatically."
echo "Check the Actions tab in your GitHub repository to monitor progress."
