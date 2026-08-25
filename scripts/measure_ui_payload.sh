#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-final}"
baseline_sha="${UI_PAYLOAD_BASE_SHA:-d9887abd2fe5f4f710176469b336e3da04d3c489}"
esbuild="$root/frontend/node_modules/.bin/esbuild"
if [[ ! -x "$esbuild" ]]; then
  echo "frontend dependencies are required (cd frontend && npm ci)" >&2
  exit 1
fi
work="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-ui-payload.XXXXXX")"
trap 'rm -rf "$work"' EXIT

case "$mode" in
  baseline)
    mkdir -p "$work/tree"
    git -C "$root" archive "$baseline_sha" internal/serveui/static | tar -x -C "$work/tree"
    static="$work/tree/internal/serveui/static"
    ;;
  final)
    static="$root/internal/serveui/static"
    ;;
  *) echo "usage: $0 baseline|final" >&2; exit 2 ;;
esac

python3 - "$mode" "$static" "$work" "$esbuild" "$baseline_sha" <<'PY'
import gzip, hashlib, json, re, subprocess, sys
from pathlib import Path
mode, static_arg, work_arg, esbuild, baseline_sha = sys.argv[1:]
static, work = Path(static_arg), Path(work_arg)


def compressed(data):
    brotli = subprocess.run(['brotli', '-q', '11', '-c'], input=data, stdout=subprocess.PIPE, check=True).stdout
    return {'raw': len(data), 'gzip6': len(gzip.compress(data, compresslevel=6, mtime=0)), 'brotli11': len(brotli)}

def add(*sizes):
    return {key: sum(size[key] for size in sizes) for key in ('raw', 'gzip6', 'brotli11')}

def rendered_html(path):
    text = path.read_text()
    text = text.replace('<meta charset="utf-8">', '<meta charset="utf-8">\n  <base href="/ui/">', 1)
    bootstrap = '<script>window.TERM_LLM_UI_PREFIX="/ui";</script><script>window.TERM_LLM_UI_VERSION="000000000000";</script><script>window.TERM_LLM_SIDEBAR_SESSIONS=["all"];</script><script>window.TERM_LLM_AGENT_NAME="";</script><script>window.TERM_LLM_AGENT_NAMES=[];</script><script>window.TERM_LLM_UI_TITLE="";</script><script>window.TERM_LLM_LOCATION_SHARING_ENABLED=true;</script><script>window.TERM_LLM_WORKTREES_ENABLED=true;</script>'
    text = text.replace('</head>', bootstrap + '</head>', 1)
    if mode == 'final':
        for name in ('icon-512.png', 'manifest.webmanifest', 'dist/app.css', 'dist/app.js'):
            text = text.replace(f'"{name}"', f'"{name}?v=000000000000"')
    else:
        vendor = ('vendor/', 'data:')
        text = re.sub(r'(?P<attr>src|href)="(?P<name>[^"?#]+\.(?:js|css|png|webmanifest))"', lambda m: m.group(0) if m.group('name').startswith(vendor) else f'{m.group("attr")}="{m.group("name")}?v=000000000000"', text)
    return text.encode()

def file_size(path): return compressed(Path(path).read_bytes())

def minify(paths, loader, output):
    combined = work / f'combined.{loader}'
    combined.write_bytes(b'\n'.join(Path(path).read_bytes() for path in paths))
    subprocess.run([esbuild, str(combined), '--minify', f'--loader:.{loader}={loader}', f'--outfile={output}', '--log-level=error'], check=True)
    return file_size(output)

html = compressed(rendered_html(static / 'index.html'))
if mode == 'baseline':
    index = (static / 'index.html').read_text()
    scripts = re.findall(r'<script src="([^"?]+)', index)
    styles = re.findall(r'<link[^>]+rel="stylesheet"[^>]+href="([^"?]+)', index)
    app_scripts = [static / name for name in scripts if not name.startswith('vendor/')]
    vendor_scripts = [static / name for name in scripts if name.startswith('vendor/')]
    app_styles = [static / name for name in styles if not name.startswith('vendor/')]
    first_js = add(*(file_size(path) for path in app_scripts))
    vendor_js = add(*(file_size(path) for path in vendor_scripts))
    first_css = add(*(file_size(path) for path in app_styles))
    min_js = minify(app_scripts, 'js', work / 'legacy.min.js')
    min_css = minify(app_styles, 'css', work / 'legacy.min.css')
    lazy_paths = [
      'vendor/katex/katex.min.css', 'vendor/katex/katex.min.js', 'vendor/katex/auto-render.min.js',
      'vendor/hljs/github-dark.min.css', 'vendor/hljs/highlight.min.js'
    ]
    lazy = add(*(file_size(static / name) for name in lazy_paths))
    delivered = {
      'html': html, 'first_party_css': first_css, 'first_party_js': first_js, 'vendor_js': vendor_js,
      'first_party_total': add(html, first_css, first_js), 'vendor_total': vendor_js,
      'combined_total': add(html, first_css, first_js, vendor_js),
      'requests': {'first_party': 1 + len(app_styles) + len(app_scripts), 'vendor': len(vendor_scripts), 'combined': 1 + len(styles) + len(scripts)},
    }
    minified = {
      'html': html, 'first_party_css': min_css, 'first_party_js': min_js, 'vendor_js': vendor_js,
      'first_party_total': add(html, min_css, min_js), 'vendor_total': vendor_js,
      'combined_total': add(html, min_css, min_js, vendor_js),
      'requests': delivered['requests'],
    }
    shell_names = ['manifest.webmanifest', 'icon-512.png'] + [str(path.relative_to(static)) for path in app_styles + app_scripts + vendor_scripts]
    result = {
      'schema': 1, 'kind': 'baseline', 'base_sha': baseline_sha,
      'conditions': {'route': '/ui/', 'webrtc': False, 'gzip_level': 6, 'brotli_quality': 11, 'html': 'representative rendered production bootstrap', 'lazy': 'KaTeX plus dark highlight theme; fonts excluded because glyph-dependent'},
      'legacy_delivered': delivered, 'legacy_toolchain_minified': minified,
      'lazy_vendor': {'assets': lazy_paths, 'sizes': lazy, 'requests': len(lazy_paths)},
      'service_worker_precache': {'assets': shell_names, 'sizes': add(*(file_size(static / name) for name in shell_names)), 'requests': len(shell_names)},
    }
else:
    app_js = file_size(static / 'dist/app.js'); app_css = file_size(static / 'dist/app.css'); vendor_js = file_size(static / 'dist/chunks/vendor.js')
    lazy_names = ['dist/chunks/rich-highlight.js', 'dist/chunks/highlight.js', 'dist/chunks/highlight.css', 'dist/chunks/rich-katex.js', 'dist/chunks/katex.js', 'dist/chunks/katex.css']
    shell_names = ['manifest.webmanifest', 'icon-512.png', 'dist/app.css', 'dist/app.js', 'dist/chunks/vendor.js']
    result = {
      'schema': 1, 'kind': 'final', 'base_sha': baseline_sha,
      'conditions': {'route': '/ui/', 'webrtc': False, 'gzip_level': 6, 'brotli_quality': 11, 'html': 'representative rendered production bootstrap', 'lazy': 'deterministic rich-highlight and rich-katex entry chunks plus their highlight and KaTeX JS/CSS dependencies; glyph-dependent KaTeX font requests are excluded in both baselines'},
      'preact': {
        'html': html, 'first_party_css': app_css, 'first_party_js': app_js, 'vendor_js': vendor_js,
        'first_party_total': add(html, app_css, app_js), 'vendor_total': vendor_js,
        'combined_total': add(html, app_css, app_js, vendor_js),
        'requests': {'first_party': 3, 'vendor': 1, 'combined': 4},
      },
      'lazy_vendor': {'assets': lazy_names, 'sizes': add(*(file_size(static / name) for name in lazy_names)), 'requests': len(lazy_names)},
      'service_worker_precache': {'assets': shell_names, 'sizes': add(*(file_size(static / name) for name in shell_names)), 'requests': len(shell_names)},
      'dist_sha256': {name: hashlib.sha256((static / name).read_bytes()).hexdigest() for name in ['dist/app.js', 'dist/app.css', 'dist/chunks/vendor.js', *lazy_names]},
    }
print(json.dumps(result, indent=2, sort_keys=True))
PY
