#!/usr/bin/env bash
# Assemble the pinned i686 CPython runtime shipped with the browser demo.
#
# The runtime is made from unmodified official Alpine v3.22 x86 packages:
#   python3 (CPython 3.12.15), musl (loader/libc), sqlite-libs.
# They are extracted into an isolated tree; no package scripts execute and the
# guest's Buildroot userland is never modified. The result is a deterministic
# tar.gz loaded into v86's 9p host store on the first shell boot.
#
# This script is build-time only. Output:
#   web/runtime/python-root.tar.gz
#   web/runtime/python-runtime.json
#   web/python-runtime.mjs
#
# Run from anywhere; it locates the experiment root from this file.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
web="$root/web"
out="$web/runtime"
work="$root/tmp/python-runtime"
mkdir -p "$out" "$work"

alpine="https://dl-cdn.alpinelinux.org/alpine/v3.22"
# name|version|package path|sha256|license|aports commit
packages=(
  "python3|3.12.15-r0|main/x86/python3-3.12.15-r0.apk|7bc3685f88e85e926f7b204c88a371079e383d913f8977fb30b453177e8c4461|PSF-2.0|18ae07d47fb83eecf3343ba1d41fddb4b70b116d"
  "musl|1.2.5-r12|main/x86/musl-1.2.5-r12.apk|d047373c9afb6eaac68df1d03128ac75803350b0368327af85cc95a5b042a1da|MIT|1546f29a1fd233db15464ce000b6247ed71b31e7"
  "sqlite-libs|3.49.2-r1|main/x86/sqlite-libs-3.49.2-r1.apk|8f02658fa7fb60a4d25b8a167fbd140bd58ea26d2d56f8ef15d8c16b3d1e6447|blessing|dac9cea7503a4e9b84d647aab2492d733ac4ac8c"
  "zlib|1.3.2-r1|main/x86/zlib-1.3.2-r1.apk|bb9844f4b3364cb3a4342f331940a3c4bdef50ed30c89c457f71bf82ce24b69f|Zlib|121fc97db0e7c59db704891b6d7ef83ae5df2cc2"
)
cpython_version="3.12.15"
cpython_source_url="https://www.python.org/ftp/python/${cpython_version}/Python-${cpython_version}.tgz"
cpython_source_sha256="de1a241a519e0a3374fea98988d0b52c886743f9d953f23be8269cc7b59c5fab"
zlib_source_url="https://zlib.net/fossils/zlib-1.3.2.tar.gz"
zlib_source_sha256="bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16"

hash_file() { sha256sum "$1" | cut -d' ' -f1; }

for spec in "${packages[@]}"; do
  IFS='|' read -r name version path expected _license _commit <<<"$spec"
  apk="$work/$name-$version.apk"
  if [[ ! -f "$apk" || "$(hash_file "$apk")" != "$expected" ]]; then
    echo "==> downloading $name $version"
    curl -fsSL --retry 3 --max-time 180 "$alpine/$path" -o "$apk"
  fi
  actual=$(hash_file "$apk")
  if [[ "$actual" != "$expected" ]]; then
    echo "SHA-256 mismatch for $name: expected $expected, got $actual" >&2
    exit 1
  fi
  extract="$work/pkg-$name"
  rm -rf "$extract"
  mkdir -p "$extract"
  tar -xzf "$apk" -C "$extract" 2>/dev/null
done

runtime="$work/root"
rm -rf "$runtime"
mkdir -p "$runtime/bin" "$runtime/lib"

cp -a "$work/pkg-python3/usr/bin/python3.12" "$runtime/bin/python3.12"
ln -s python3.12 "$runtime/bin/python3"
cp -a "$work/pkg-python3/usr/lib/libpython3.12.so.1.0" "$runtime/lib/"
ln -s libpython3.12.so.1.0 "$runtime/lib/libpython3.12.so.1"
ln -s libpython3.12.so.1.0 "$runtime/lib/libpython3.12.so"
cp -a "$work/pkg-python3/usr/lib/libpython3.so" "$runtime/lib/"
cp -a "$work/pkg-python3/usr/lib/python3.12" "$runtime/lib/python3.12"
cp -a "$work/pkg-musl/lib/ld-musl-i386.so.1" "$runtime/lib/"
cp -a "$work/pkg-musl/lib/libc.musl-x86.so.1" "$runtime/lib/"
cp -a "$work/pkg-sqlite-libs/usr/lib/libsqlite3.so.3.49.2" "$runtime/lib/"
ln -s libsqlite3.so.3.49.2 "$runtime/lib/libsqlite3.so.0"
cp -a "$work/pkg-zlib/usr/lib/libz.so.1.3.2" "$runtime/lib/"
ln -s libz.so.1.3.2 "$runtime/lib/libz.so.1"

# Keep the real standard library, including unittest and sqlite3. Drop only
# developer-only payloads that would imply unavailable capabilities: ensurepip's
# bundled pip wheel (there is no pip/package installation), lib2to3, and the
# turtle demo. site-packages is empty in the package; do not ship it.
rm -rf "$runtime/lib/python3.12/ensurepip" \
       "$runtime/lib/python3.12/lib2to3" \
       "$runtime/lib/python3.12/turtledemo" \
       "$runtime/lib/python3.12/site-packages" \
       "$runtime/lib/python3.12/EXTERNALLY-MANAGED"
find "$runtime" -type d -name __pycache__ -prune -exec rm -rf {} +
find "$runtime" -name '*.pyc' -delete

# Deterministic tar: sorted names, zero ownership/timestamps, no gzip mtime.
archive="$out/python-root.tar.gz"
tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner --format=gnu \
  -cf - -C "$runtime" . | gzip -n -9 >"$archive"
sha=$(hash_file "$archive")
bytes=$(stat -c '%s' "$archive")
installed_bytes=$(find "$runtime" -type f -printf '%s\n' | awk '{s+=$1} END {print s+0}')
files=$(find "$runtime" -type f | wc -l | tr -d ' ')

python3 - "$out/python-runtime.json" "$sha" "$bytes" "$installed_bytes" "$files" \
  "$cpython_version" "$cpython_source_url" "$cpython_source_sha256" \
  "$zlib_source_url" "$zlib_source_sha256" "${packages[@]}" <<'PY'
import json, sys
out, sha, size, installed, count, pyver, source, source_sha, zlib_source, zlib_sha, *specs = sys.argv[1:]
packages = []
for spec in specs:
    name, version, path, digest, license_, commit = spec.split('|')
    packages.append({
        'name': name,
        'version': version,
        'url': 'https://dl-cdn.alpinelinux.org/alpine/v3.22/' + path,
        'sha256': digest,
        'license': license_,
        'aports_commit': commit,
        'aports_source': 'https://gitlab.alpinelinux.org/alpine/aports/-/tree/' + commit,
    })
manifest = {
    'python_version': pyver,
    'archive': 'python-root.tar.gz',
    'sha256': sha,
    'bytes': int(size),
    'installed_bytes': int(installed),
    'files': int(count),
    'packages': packages,
    'cpython_source': {'url': source, 'sha256': source_sha},
    'zlib_source': {'url': zlib_source, 'sha256': zlib_sha},
    'notes': [
        'Unmodified Alpine v3.22 x86 package payloads; no package scripts execute.',
        'Bundled shared libraries: musl, libsqlite3 and zlib. No SSL/readline/etc.',
        'Tested: json, csv, statistics, unittest and sqlite3; base64/binascii/gzip/zipfile work via zlib.',
        'Optional extension modules with unbundled libraries are unavailable: ssl/libssl, ctypes/libffi, bz2, lzma, readline, curses, pyexpat.',
        'decimal and hashlib use built-in/pure-Python fallbacks and remain importable.',
        'ensurepip, lib2to3, turtledemo and empty site-packages are omitted; no pip or third-party packages.',
        'Python is run with -B/PYTHONDONTWRITEBYTECODE=1; the runtime tree is not written to.',
        'No pip, network, package installation, or host execution is provided.',
    ],
}
with open(out, 'w', encoding='utf-8') as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write('\n')
PY

cat >"$web/python-runtime.mjs" <<EOF
// Generated by scripts/fetch-python-runtime.sh. Do not edit by hand.
// See web/runtime/python-runtime.json and LINUX-SOURCES.md for provenance.
export const pythonRuntime = {
  file: 'runtime/python-root.tar.gz',
  sha256: '$sha',
  bytes: $bytes,
  installedBytes: $installed_bytes,
  files: $files,
  pythonVersion: '$cpython_version',
};
EOF

echo "==> wrote $archive"
echo "    sha256=$sha bytes=$bytes installed=$installed_bytes files=$files"
