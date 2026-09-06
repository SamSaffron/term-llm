# term-llm site

The product homepage and documentation are built with **Hugo 0.145.0**. The site is static: no frontend framework, hosted search service, remote fonts, or runtime package installation is required in production.

## Develop

Run from the repository root with Hugo and Node.js 24 or newer installed:

```bash
npm --prefix docs-site ci
hugo server --source docs-site
```

Hugo previews do not generate the Pagefind search index. To exercise search, build and serve the production output:

```bash
npm --prefix docs-site run build
python3 -m http.server 1313 --bind 127.0.0.1 --directory .cache/docs-site
```

The build writes to `.cache/docs-site/`, including `/pagefind/` assets. `package-lock.json` pins the search builder and browser validation tools. Deployment uses the same build and validation commands before publishing.

## Validate

Install the test browser once (on Linux CI, use `--with-deps` to install system libraries too):

```bash
cd docs-site
npx playwright install chromium
cd ..
npm --prefix docs-site test
```

The test command builds Hugo and Pagefind, starts an ephemeral loopback-only static server, and checks:

- every generated page’s local links, fragments, assets, heading IDs, and metadata;
- light/dark OS preferences, a persisted explicit override, and matching product images;
- copy controls, keyboard-operated install tabs, real search results, modal focus, and shortcuts;
- documentation navigation, preserved provider section links, and reduced-motion behavior;
- layouts at 320, 390, 768, 1024, and 1440 pixels;
- representative pages with axe WCAG A/AA checks in desktop/mobile and light/dark modes;
- no-JavaScript navigation and installation, and unavailable browser storage.

Screenshots are written to the ignored `docs-site/test-results/` directory. Automated checks supplement—not replace—visual inspection and manual assistive-technology testing. No model requests or credentials are needed.

## Content and layout

- Author documentation in Markdown under `content/`.
- Homepage copy and examples live in `content/_index.md`; `layouts/index.html` supplies the presentation.
- `data/navigation.yaml` groups the documentation and drives both the sidebar and guides index. Add new pages to the appropriate group.
- Detailed provider settings live in `reference/provider-setup-details.md`. The old getting-started fragment IDs remain as links to the moved sections.
- `static/styles.css` defines the shared visual tokens. Light is the baseline; dark follows the OS unless a reader explicitly chooses Light or Dark. Search, syntax colors, and screenshots follow the same preference.
- `static/site.js` progressively enhances native navigation, code copying, install tabs, and the search dialog. Installation commands and navigation remain usable without JavaScript.
- Code fences use a Hugo render hook for language labels and copy controls. Optional filenames use attributes, for example:

````markdown
```yaml {title="config.yaml"}
default_provider: zen
```
````

Avoid static lists of “currently free” models in the quickstart: provider catalogs and service availability change. Link to model discovery and provider-specific reference instead.

## Product and social images

The homepage uses **screenshots of the real term-llm browser interface**, not a hand-drawn approximation. The conversation and file-change API responses are deterministic illustrative fixtures, clearly labeled on the site. No customer data or real credentials are included. The initial captures were made with the installed v0.9.30 interface.

To refresh them, start an **isolated** loopback-only web runtime. Do not point the capture script at a personal session or public service. For example, from the repository root:

```bash
# Use a temporary HOME and config; never your normal credentials or session store.
demo_home="$(mktemp -d)"
mkdir -p "$demo_home/config/term-llm" "$demo_home/workspace"
printf 'default_provider: debug\nproviders:\n  debug:\n    model: fast\nserve:\n  auto_title: false\n' > "$demo_home/config/term-llm/config.yaml"
binary="$(command -v term-llm)"
(
  cd "$demo_home/workspace"
  exec env -i PATH="$PATH" HOME="$demo_home" \
    XDG_CONFIG_HOME="$demo_home/config" XDG_DATA_HOME="$demo_home/data" \
    XDG_CACHE_HOME="$demo_home/cache" \
    "$binary" serve web --host 127.0.0.1 --port 18765 --no-auth --approval prompt
) &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null; wait "$server_pid" 2>/dev/null; rm -rf "$demo_home"' EXIT
# Once the server prints its listening URL:
npm --prefix docs-site run capture:product
```

`capture-product.mjs` intercepts the API with fixture data and captures both OS themes. It rejects non-loopback URLs. Set `DOCS_PRODUCT_URL` if using a different local port or base path. Check both captures visually after an app UI update.

### Homepage product tour

The five-scene carousel is configured in `content/_index.md` under `web.slides`.
Images live in `static/images/tour/`, with matching light/dark WebP variants. Lossless 2560×1360 originals keep UI text
pixel-identical; high-quality 900px previews use responsive `srcset` delivery.
The browser chooses the appropriate resolution for its viewport and pixel density.
The existing `web-workspace-*.png` images remain available for documentation pages.

Capture/optimization requires Python 3 with Pillow (`python3 -m pip install Pillow`)
in addition to the existing Node/Playwright tools. Capture PNGs are kept in ignored
`test-results/tour-source/`; `scripts/optimize-tour.py` produces the deployed WebPs.
The build does not require Pillow; optimized assets are committed.

To regenerate the tour, use the isolated web server setup above and also start
an isolated Hub before capturing. Run this in the same shell, while the web
server and `demo_home` still exist:

```bash
(
  cd "$demo_home/workspace"
  exec env -i PATH="$PATH" HOME="$demo_home" \
    XDG_CONFIG_HOME="$demo_home/config" XDG_DATA_HOME="$demo_home/hub-data" \
    XDG_CACHE_HOME="$demo_home/cache" \
    "$binary" serve hub --host 127.0.0.1 --port 18766 --auth none
) &
hub_pid=$!
trap 'kill "$server_pid" "$hub_pid" 2>/dev/null; wait "$server_pid" "$hub_pid" 2>/dev/null; rm -rf "$demo_home"' EXIT
# Once both servers are listening:
npm --prefix docs-site run capture:tour
```

`DOCS_PRODUCT_URL` and `DOCS_HUB_URL` override the two loopback URLs. The capture
script uses fixture APIs and an isolated terminal-output stream; it makes no
model calls or real cross-node delegations. The illustrative activity label on
the homepage must remain. Inspect both themes after changing fixtures or the UI.

The tour slides every five seconds while its picture area is visible, including
when the pointer rests over it. Tabs and dots select a scene and restart the
interval. The small icon button explicitly pauses/resumes rotation. Keyboard
interaction and image enlargement pause it; reduced motion disables automatic
rotation and slide animation. Horizontal mobile swipes move between scenes,
while vertical gestures scroll the page. Inert edge copies make looping seamless.
Without JavaScript, the opening image and links to all five originals remain.
`npm test` covers these behaviors, native touch input, theme assets, image-size
budgets, enlargement, and compact MacBook/mobile viewports.

Regenerate the 1200×630 social image independently:

```bash
node docs-site/scripts/capture-social.mjs
```
