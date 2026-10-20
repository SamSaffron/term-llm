# Browser Linux sandbox provenance

The emulator is v86 npm `0.5.458+gd96be77`, source commit
`d96be774e549a83371b038b86e819804c96b921f`, BSD-2-Clause.
`package-lock.json` pins the package integrity. Its license accompanies the demo.
Source: https://github.com/copy/v86/tree/d96be774e549a83371b038b86e819804c96b921f

Firmware and the Linux/Buildroot image are fetched directly by the visitor's
browser from upstream, with SHA-256 verification before execution. They are not
committed or redistributed by this demo host. Exact URLs and hashes are in
`web/linux-assets.mjs` (reused from the existing browser Linux tutorial).

The kernel identifies as Linux 6.8.12; the exact corresponding Buildroot
configuration/source for the upstream image has not been established. Generic
Linux or Buildroot links are not proof of complete corresponding source. Do not
mirror this image or firmware without resolving distribution obligations.

## CPython runtime overlay

Python is real CPython 3.12.15, not MicroPython. The runtime is assembled from
unmodified official Alpine v3.22 x86 packages by
`scripts/fetch-python-runtime.sh`:

| Package | Version | License | Alpine aports commit |
|---|---|---|---|
| `python3` | 3.12.15-r0 | PSF-2.0 | `18ae07d47fb83eecf3343ba1d41fddb4b70b116d` |
| `musl` | 1.2.5-r12 | MIT | `1546f29a1fd233db15464ce000b6247ed71b31e7` |
| `sqlite-libs` | 3.49.2-r1 | blessing (public domain) | `dac9cea7503a4e9b84d647aab2492d733ac4ac8c` |
| `zlib` | 1.3.2-r1 | Zlib | `121fc97db0e7c59db704891b6d7ef83ae5df2cc2` |

The exact package URLs, SHA-256 digests, versions, and aports links are in
`web/runtime/python-runtime.json`; the generated `web/python-runtime.mjs`
contains the output archive hash and is checked before upload. CPython 3.12.15
source is published at
`https://www.python.org/ftp/python/3.12.15/Python-3.12.15.tgz`
(SHA-256 `de1a241a519e0a3374fea98988d0b52c886743f9d953f23be8269cc7b59c5fab`);
zlib 1.3.2 source is at `https://zlib.net/fossils/zlib-1.3.2.tar.gz`
(SHA-256 `bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16`).
The corresponding Alpine APKBUILD, patches, and install scripts are available
at the aports commits above. `ensurepip` (its bundled pip wheel), `lib2to3`,
`turtledemo`, empty `site-packages`, and bytecode caches are omitted. The
pure-Python standard library plus the tested extension modules `json`, `csv`,
`statistics`, `unittest`, and `sqlite3` are present. Bundled shared libraries
are musl, `libsqlite3`, and zlib; zlib makes `binascii`, `zlib`, `base64`,
`gzip`, and `zipfile` work. This is not a claim that every optional extension
works. `ssl`, `ctypes`, `bz2`, `lzma`, `readline`, `curses`, and `pyexpat` fail
to import because their libraries are absent. `decimal` and `hashlib` fall back
to built-in/pure-Python implementations, and `xml.etree.ElementTree` uses its
pure fallback. `readline` is deliberately not bundled, avoiding a GPL dependency
and its corresponding-source obligation. There is no pip, package installation,
network, or host execution. Notices are in `licenses/` and are copied into
`dist/licenses/`.

The asset is a same-origin static file fetched on the first shell boot. It is
uploaded through v86's host-backed 9p store (not guest RAM), verified by
SHA-256, and extracted to `/mnt/runtime/python`; the archive is then removed.
The 7.3 MiB archive expands to about 22.6 MiB / 613 files. A measured CPython
run with JSON/CSV/statistics/unittest/sqlite3 used about 13 MiB RSS in the
64 MiB guest and started in tens of milliseconds once extracted. `/tmp/python`
is a wrapper that sets `PYTHONHOME`, `PYTHONDONTWRITEBYTECODE=1`, and `-B`, so
the installed runtime is not written to.

## Guest limits

The guest has 64 MiB configured RAM (about 39 MiB usable `MemTotal`, with the
Buildroot initramfs occupying the rest), no network device, a serial console,
and a 9p shared text workspace. Go/WASM owns the authoritative ephemeral
workspace; each command synchronizes it into the guest and returns a validated
snapshot. The VM pauses between calls. Reload resets everything. Cancellation or
VM errors discard the guest, retaining the last authoritative Go workspace
snapshot.

This is a root-access disposable Linux guest, not a host shell. BusyBox, shell,
awk, and CPython 3.12 are available; Node, Go, pip, and a package manager are
not. Workspace file tools accept regular UTF-8 text files only, up to 256 files
/ 8 MiB total; binary files, symlinks, and special files cannot be synchronized.
Guest filesystem changes outside `/workspace` remain guest-local until reload.
The 9p backing store has separate host-side allocation/inode limits; guest RAM
alone would not bound that memory. The shared workspace/bridge retains its
16 MiB cap. The Python overlay has a separate, explicit 32 MiB cap; both are
bounded, and the runtime archive is deleted after extraction. Commands default
to 10 seconds (maximum 30), with a separate cold boot budget that includes the
one-time Python runtime download/extraction. YOLO removes approval prompts, not
browser/resource limits.
