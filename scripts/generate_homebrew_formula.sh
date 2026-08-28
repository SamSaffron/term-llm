#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: $0 <release-tag> <checksums-file> <output-file>" >&2
    exit 2
fi

tag=$1
checksums_file=$2
output_file=$3

case "$tag" in
    v[0-9]*) ;;
    *)
        echo "release tag must start with v followed by a digit: $tag" >&2
        exit 1
        ;;
esac

[ -f "$checksums_file" ] || {
    echo "checksums file not found: $checksums_file" >&2
    exit 1
}

version=${tag#v}

checksum_for() {
    asset=$1
    checksum=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$checksums_file")
    if ! printf '%s\n' "$checksum" | grep -Eq '^[0-9a-f]{64}$'; then
        echo "missing or invalid checksum for $asset" >&2
        exit 1
    fi
    printf '%s\n' "$checksum"
}

darwin_arm64_asset="term-llm_${version}_darwin_arm64.tar.gz"
darwin_amd64_asset="term-llm_${version}_darwin_amd64.tar.gz"
linux_arm64_asset="term-llm_${version}_linux_arm64.tar.gz"
linux_amd64_asset="term-llm_${version}_linux_amd64.tar.gz"

darwin_arm64_sha=$(checksum_for "$darwin_arm64_asset")
darwin_amd64_sha=$(checksum_for "$darwin_amd64_asset")
linux_arm64_sha=$(checksum_for "$linux_arm64_asset")
linux_amd64_sha=$(checksum_for "$linux_amd64_asset")

mkdir -p "$(dirname "$output_file")"
tmp_file="${output_file}.tmp"
trap 'rm -f "$tmp_file"' EXIT INT TERM HUP

cat >"$tmp_file" <<EOF
class TermLlm < Formula
  desc "Terminal-first AI runtime for commands, chat, editing, tools, jobs, and agents"
  homepage "https://term-llm.com"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SamSaffron/term-llm/releases/download/$tag/$darwin_arm64_asset"
      sha256 "$darwin_arm64_sha"
    end

    on_intel do
      url "https://github.com/SamSaffron/term-llm/releases/download/$tag/$darwin_amd64_asset"
      sha256 "$darwin_amd64_sha"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SamSaffron/term-llm/releases/download/$tag/$linux_arm64_asset"
      sha256 "$linux_arm64_sha"
    end

    on_intel do
      url "https://github.com/SamSaffron/term-llm/releases/download/$tag/$linux_amd64_asset"
      sha256 "$linux_amd64_sha"
    end
  end

  def install
    bin.install "term-llm"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/term-llm version")
  end
end
EOF

mv "$tmp_file" "$output_file"
trap - EXIT INT TERM HUP
