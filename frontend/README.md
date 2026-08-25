# Chat Web UI frontend

The embedded chat application is authored in Preact and strict TypeScript under `frontend/src`. Vite emits deterministic, minified production files into `internal/serveui/static/dist`; those generated files are committed and embedded in the Go binary. Normal `go build`, release builds, and binary users do not need Node.

## Prerequisites

Frontend contributors use Node 24 or newer and npm. The test setup supplies isolated Web Storage implementations for Node 25+, whose process-level `localStorage`/`sessionStorage` globals otherwise depend on runtime flags. CI installs dependencies from `package-lock.json` with `npm ci`.

```sh
cd frontend
npm ci
```

## Commands

```sh
npm run typecheck       # strict TypeScript check
npm test                # Vitest domain/store/component tests
npm run build           # regenerate static/dist
npm run check:generated # rebuild and reject a stale committed dist
npm run test:e2e        # Playwright tests against TERM_LLM_SMOKE_URL
```

From the repository root, `scripts/browser_lifecycle_smoke.sh` builds and starts a temporary `term-llm serve web --no-auth` process and runs the Playwright chat suite. Run `npm ci` in `frontend/` first; the standalone smoke fails immediately with that instruction rather than downloading dependencies implicitly. Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/chromium` to use a system browser locally. CI installs the Playwright-pinned browser.

`scripts/check_frontend_network_policy.sh` fails closed unless ripgrep can scan production sources and enforces transport ownership for `fetch`, browser fetch aliases, XHR, EventSource, WebSocket, and beacon calls. Raw transports are allowed only under `src/api` and in the reviewed WebRTC platform bridge. Run `scripts/measure_ui_payload.sh baseline|final` to reproduce the raw/gzip-6/Brotli-11 payload data. The script derives initial module requests and service-worker cache candidates from generated HTML/imports and `SHELL_ASSETS`; the service worker performs zero install-time asset requests. The fixed baseline and current generated results live in `payload-baseline.json` and `payload-final.json`; `payload-report.md` explains inclusion rules and deltas.

## Build contract

- Relative Vite base (`./`) preserves direct `/chat` and Hub node mounts.
- Stable output names use `dist/app.js`, `dist/app.css`, `dist/chunks/*.js`, `dist/chunks/*.css`, and `dist/assets/*`.
- Marked, DOMPurify, Preact, and Signals form the separately reported initial vendor chunk.
- KaTeX and highlight.js are deterministic lazy chunks. KaTeX emits only WOFF2 fonts.
- Source maps, manifests, hashes in filenames, and frontend test artifacts are not emitted under `static`.
- `AssetVersion()` query parameters remain the cache-busting mechanism for HTML-linked shell assets. Stable-named Vite chunks are imported without that query and therefore remain network-first in the service worker.

Do not edit generated `static/dist` files directly. Change TypeScript/CSS, run `npm run build`, inspect the output, and commit source and generated files together.
