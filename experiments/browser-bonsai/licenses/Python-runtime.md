# Guest CPython runtime

The demo ships a small i686 CPython runtime so the real Linux guest can execute
Python without network, pip, or host execution. It is assembled at build time
from unmodified official Alpine v3.22 x86 packages; no package install scripts
execute, and the guest's Buildroot userland and libc are not replaced.

| Package | Version | License | Alpine aports commit |
|---|---|---|---|
| `python3` (CPython) | 3.12.15-r0 | PSF-2.0 | `18ae07d47fb83eecf3343ba1d41fddb4b70b116d` |
| `musl` (loader/libc) | 1.2.5-r12 | MIT | `1546f29a1fd233db15464ce000b6247ed71b31e7` |
| `sqlite-libs` | 3.49.2-r1 | blessing (public domain) | `dac9cea7503a4e9b84d647aab2492d733ac4ac8c` |
| `zlib` | 1.3.2-r1 | Zlib | `121fc97db0e7c59db704891b6d7ef83ae5df2cc2` |

Exact package URLs, SHA-256 digests, versions, and aports links are recorded in
`experiments/browser-bonsai/web/runtime/python-runtime.json` and embedded in the
generated `web/python-runtime.mjs`. The corresponding CPython 3.12.15 source is
published by the Python Software Foundation at:

```
https://www.python.org/ftp/python/3.12.15/Python-3.12.15.tgz
SHA-256 de1a241a519e0a3374fea98988d0b52c886743f9d953f23be8269cc7b59c5fab
```

zlib 1.3.2 source is published at:

```
https://zlib.net/fossils/zlib-1.3.2.tar.gz
SHA-256 bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16
```

Alpine package sources (APKBUILD, patches, and install scripts) are available
from the aports commits above, for example
`https://gitlab.alpinelinux.org/alpine/aports/-/tree/18ae07d47fb83eecf3343ba1d41fddb4b70b116d/main/python3`.
The binaries in the runtime are the unmodified package payloads. `ensurepip`
(its bundled pip wheel), `lib2to3`, `turtledemo`, empty `site-packages`, and
bytecode caches are omitted. The pure-Python standard library plus the tested
extension modules `json`, `csv`, `statistics`, `unittest`, and `sqlite3` are
present. Bundled shared libraries are musl, `libsqlite3`, and zlib (for
`binascii`, `zlib`, `base64`, `gzip`, and `zipfile`). This is not a claim that
every optional extension module works. The following optional modules require
unbundled system libraries and fail to import: `ssl`/`_ssl` (libssl, libcrypto),
`ctypes` (libffi), `bz2` (libbz2), `lzma` (liblzma), `readline` (libreadline,
deliberately excluded to avoid GPL dependencies), `curses` (ncursesw/panelw),
and `pyexpat` (libexpat). `decimal` and `hashlib` remain importable through
their built-in/pure-Python fallbacks, and `xml.etree.ElementTree` uses its
pure-Python fallback. SQLite is the only database extension included.

Notices:

- `Python-LICENSE.txt` – CPython's PSF license, copied from the package's
  `lib/python3.12/LICENSE.txt`.
- `musl-COPYRIGHT.txt` – musl MIT license.
- `zlib-LICENSE.txt` – zlib license.
- SQLite is in the public domain; the Alpine package declares the `blessing`
  license. Its source is https://www.sqlite.org/.

The runtime is fetched as a same-origin static asset on the first shell boot,
verified by SHA-256, unpacked into v86's host-backed 9p store at
`/mnt/runtime/python`, and run with the bundled musl loader. It is not copied
into guest RAM: a measured CPython run with `json`, `csv`, `statistics`,
`unittest`, and `sqlite3` used about 13 MiB RSS on the 64 MiB guest. A
`/tmp/python` wrapper sets `PYTHONHOME`, `PYTHONDONTWRITEBYTECODE=1`, and `-B`
so the installed runtime is not written to. There is no pip, package
installation, network, or host execution.
