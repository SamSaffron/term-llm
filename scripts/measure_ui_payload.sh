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
  final|hub)
    static="$root/internal/serveui/static"
    ;;
  *) echo "usage: $0 baseline|final|hub" >&2; exit 2 ;;
esac

python3 - "$mode" "$static" "$work" "$esbuild" "$baseline_sha" <<'PY'
import gzip, hashlib, json, re, subprocess, sys
from pathlib import Path
from urllib.parse import urlsplit
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
    return text


def local_name(reference):
    return urlsplit(reference).path.removeprefix('./').removeprefix('/')


def html_assets(text):
    scripts = [local_name(value) for value in re.findall(r'<script[^>]+src="([^"]+)', text)]
    styles = [local_name(value) for value in re.findall(r'<link[^>]+rel="stylesheet"[^>]+href="([^"]+)', text)]
    return scripts, styles


def static_imports(name):
    source = (static / name).read_text()
    references = re.findall(r'(?:\bfrom|\bimport)\s*(?:[^"\']*?\sfrom\s*)?["\']([^"\']+)["\']', source)
    parent = Path(name).parent
    output = []
    for reference in references:
        if not reference.startswith('.'):
            continue
        resolved = (parent / reference).as_posix()
        while '/./' in resolved:
            resolved = resolved.replace('/./', '/')
        output.append(str(Path(resolved)))
    return output


def initial_graph(entries):
    found, pending = [], list(entries)
    while pending:
        name = pending.pop(0)
        if name in found or not (static / name).is_file():
            continue
        found.append(name)
        if name.endswith(('.js', '.mjs')):
            pending.extend(static_imports(name))
    return found


def file_size(path):
    return compressed(Path(path).read_bytes())


def sum_names(names):
    return add(*(file_size(static / name) for name in names)) if names else add()


def minify(paths, loader, output):
    combined = work / f'combined.{loader}'
    combined.write_bytes(b'\n'.join(Path(path).read_bytes() for path in paths))
    subprocess.run([esbuild, str(combined), '--minify', f'--loader:.{loader}={loader}', f'--outfile={output}', '--log-level=error'], check=True)
    return file_size(output)


def service_worker_candidates():
    source = (static / 'sw.js').read_text()
    match = re.search(r'const\s+SHELL_ASSETS\s*=\s*\[([\s\S]*?)\];', source)
    if not match:
        raise SystemExit('could not derive SHELL_ASSETS from service worker')
    names = []
    for reference in re.findall(r'["\']([^"\']+)["\']', match.group(1)):
        name = local_name(reference)
        if (static / name).is_file() and name not in names:
            names.append(name)
    return names


if mode == 'hub':
    names = ['dist/hub.js', 'dist/hub.css']
    missing = [name for name in names if not (static / name).is_file()]
    if missing:
        raise SystemExit(f'missing generated Hub assets: {missing}')
    shell_text = '<!doctype html><html><head><link rel="stylesheet" href="/hub/dist/hub.css?v=000000000000"><script type="module" src="/hub/dist/hub.js"></script></head><body><div id="root" data-hub-config="{}"></div></body></html>'
    html = compressed(shell_text.encode())
    shell_assets = service_worker_candidates()
    if any(name in shell_assets for name in names):
        raise SystemExit('Hub assets must not join the chat service-worker cache')
    result = {
        'schema': 1,
        'kind': 'hub',
        'conditions': {
            'route': '/hub/',
            'gzip_level': 6,
            'brotli_quality': 11,
            'html': 'representative escaped Hub bootstrap shell',
            'graph': 'standalone hub.js and hub.css; HTML excluded from the two asset requests',
        },
        'html': html,
        'assets': {name: file_size(static / name) for name in names},
        'asset_total': sum_names(names),
        'asset_requests': 2,
        'initial_assets': names,
        'dist_sha256': {name: hashlib.sha256((static / name).read_bytes()).hexdigest() for name in names},
    }
    print(json.dumps(result, indent=2, sort_keys=True))
    raise SystemExit(0)


html_text = rendered_html(static / 'index.html')
html = compressed(html_text.encode())
scripts, styles = html_assets(html_text)
shell_names = service_worker_candidates()
shell = {
    'strategy': 'opportunistic cache allowlist; install performs no asset fetches',
    'assets': shell_names,
    'candidate_sizes': sum_names(shell_names),
    'install_requests': 0,
}

if mode == 'baseline':
    app_scripts = [static / name for name in scripts if not name.startswith('vendor/')]
    vendor_scripts = [static / name for name in scripts if name.startswith('vendor/')]
    app_styles = [static / name for name in styles if not name.startswith('vendor/')]
    first_js = add(*(file_size(path) for path in app_scripts)); vendor_js = add(*(file_size(path) for path in vendor_scripts)); first_css = add(*(file_size(path) for path in app_styles))
    min_js = minify(app_scripts, 'js', work / 'legacy.min.js'); min_css = minify(app_styles, 'css', work / 'legacy.min.css')
    lazy_paths = ['vendor/katex/katex.min.css', 'vendor/katex/katex.min.js', 'vendor/katex/auto-render.min.js', 'vendor/hljs/github-dark.min.css', 'vendor/hljs/highlight.min.js']
    requests = {'first_party': 1 + len(app_styles) + len(app_scripts), 'vendor': len(vendor_scripts)}; requests['combined'] = requests['first_party'] + requests['vendor']
    delivered = {'html': html, 'first_party_css': first_css, 'first_party_js': first_js, 'vendor_js': vendor_js, 'first_party_total': add(html, first_css, first_js), 'vendor_total': vendor_js, 'combined_total': add(html, first_css, first_js, vendor_js), 'requests': requests}
    minified = {'html': html, 'first_party_css': min_css, 'first_party_js': min_js, 'vendor_js': vendor_js, 'first_party_total': add(html, min_css, min_js), 'vendor_total': vendor_js, 'combined_total': add(html, min_css, min_js, vendor_js), 'requests': requests}
    result = {
        'schema': 2, 'kind': 'baseline', 'base_sha': baseline_sha,
        'conditions': {'route': '/ui/', 'webrtc': False, 'gzip_level': 6, 'brotli_quality': 11, 'html': 'representative rendered production bootstrap', 'lazy': 'KaTeX plus dark highlight theme; fonts excluded because glyph-dependent'},
        'legacy_delivered': delivered, 'legacy_toolchain_minified': minified,
        'lazy_vendor': {'assets': lazy_paths, 'sizes': sum_names(lazy_paths), 'requests': len(lazy_paths)},
        'service_worker_cache': shell,
    }
else:
    graph = initial_graph(scripts)
    vendor_names = [name for name in graph if '/vendor.' in name or name.endswith('/vendor.js')]
    app_names = [name for name in graph if name.endswith(('.js', '.mjs')) and name not in vendor_names]
    style_names = [name for name in styles if (static / name).is_file()]
    lazy_names = sorted(path.relative_to(static).as_posix() for path in (static / 'dist/chunks').glob('*') if path.is_file() and path.suffix in ('.js', '.css') and path.name.startswith(('rich-', 'highlight', 'katex')))
    request_counts = {'first_party': 1 + len(style_names) + len(app_names), 'vendor': len(vendor_names)}; request_counts['combined'] = request_counts['first_party'] + request_counts['vendor']
    app_js, app_css, vendor_js = sum_names(app_names), sum_names(style_names), sum_names(vendor_names)
    hash_names = sorted(set(app_names + style_names + vendor_names + lazy_names))
    result = {
        'schema': 2, 'kind': 'final', 'base_sha': baseline_sha,
        'conditions': {'route': '/ui/', 'webrtc': False, 'gzip_level': 6, 'brotli_quality': 11, 'html': 'representative rendered production bootstrap', 'lazy': 'generated rich-highlight and rich-katex chunks plus generated highlight/KaTeX JS/CSS; glyph-dependent fonts excluded'},
        'preact': {'html': html, 'first_party_css': app_css, 'first_party_js': app_js, 'vendor_js': vendor_js, 'first_party_total': add(html, app_css, app_js), 'vendor_total': vendor_js, 'combined_total': add(html, app_css, app_js, vendor_js), 'requests': request_counts, 'initial_assets': {'first_party': style_names + app_names, 'vendor': vendor_names}},
        'lazy_vendor': {'assets': lazy_names, 'sizes': sum_names(lazy_names), 'requests': len(lazy_names)},
        'service_worker_cache': shell,
        'dist_sha256': {name: hashlib.sha256((static / name).read_bytes()).hexdigest() for name in hash_names},
    }
print(json.dumps(result, indent=2, sort_keys=True))
PY
