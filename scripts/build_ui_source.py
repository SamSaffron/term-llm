#!/usr/bin/env python3
"""Build the deterministic, private readable-source archive for UI authoring."""
import gzip
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile

root = Path(__file__).resolve().parent.parent
files = {}
for p in sorted((root / 'frontend/src').rglob('*')):
    rel = p.relative_to(root).as_posix()
    if not p.is_file() or p.is_symlink() or '/hub/' in rel or '/test/' in rel or '.test.' in rel:
        continue
    if p.suffix in ('.ts', '.tsx', '.css'):
        files[rel] = p.read_bytes()
for rel in ['frontend/README.md', 'internal/agents/builtin/extension-builder/system.md']:
    p = root / rel
    if p.exists():
        files[rel] = p.read_bytes()
assets = hashlib.sha256()
for p in sorted((root / 'internal/serveui/static').rglob('*')):
    if not p.is_file() or p.name in ('hub.js', 'hub.css'):
        continue
    rel = p.relative_to(root / 'internal/serveui').as_posix()
    if rel.startswith('static/dist/') or rel in ['static/index.html', 'static/manifest.webmanifest', 'static/icon-512.png', 'static/sw.js']:
        assets.update(rel.encode() + b'\0' + p.read_bytes() + b'\0')
source_digest = hashlib.sha256()
for rel, data in sorted(files.items()):
    source_digest.update(rel.encode() + b'\0' + data + b'\0')
try:
    version = subprocess.check_output(['git', 'describe', '--tags', '--always'], cwd=root, text=True).strip()
except subprocess.CalledProcessError:
    version = 'dev'
manifest = {'format_version': 1, 'version': version, 'asset_version': assets.hexdigest()[:12], 'source_digest': source_digest.hexdigest(), 'files': {p: {'size': len(b), 'sha256': hashlib.sha256(b).hexdigest()} for p, b in sorted(files.items())}}
files['source-manifest.json'] = (json.dumps(manifest, sort_keys=True, indent=2) + '\n').encode()
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode='w', format=tarfile.USTAR_FORMAT) as archive:
    for rel, data in sorted(files.items()):
        info = tarfile.TarInfo(rel)
        info.size = len(data)
        info.mode = 0o644
        archive.addfile(info, io.BytesIO(data))
out = root / 'internal/uisource/assets/term-llm-ui-source.tar.gz'
out.parent.mkdir(parents=True, exist_ok=True)
compressed = io.BytesIO()
with gzip.GzipFile(fileobj=compressed, mode='wb', filename='', mtime=0) as zipped:
    zipped.write(buf.getvalue())
out.write_bytes(compressed.getvalue())
print(f'UI authoring source: {len(files)} files, {out.stat().st_size:,} compressed bytes')
