# Embedded Web UI frontends

The chat application and Hub UI are authored in Preact and strict TypeScript under `frontend/src`. The Hub source of truth is `frontend/src/hub/`; its dashboard, bearer login, passkey setup/login/recovery, security administration, behavior, and styling must not be reintroduced into Go templates. Hub HTTP transport is owned by `frontend/src/api/hub-client.ts`.

Vite emits deterministic, minified production files into `internal/serveui/static/dist`. That generated directory is ignored by Git and embedded in the Go binary, so source builds require the frontend build while release binaries remain self-contained.

## Prerequisites

Source builds use Node 24 or newer and npm. The test setup supplies isolated Web Storage implementations for Node 25+, whose process-level `localStorage`/`sessionStorage` globals otherwise depend on runtime flags. CI installs dependencies from `package-lock.json` with `npm ci`.

From the repository root, the normal build command installs locked frontend dependencies when needed, generates both applications, and builds `./term-llm`:

```sh
make build
```

## Commands

```sh
make frontend             # chat build, then standalone Hub build
npm --prefix frontend run format:check
npm --prefix frontend run lint
npm --prefix frontend run typecheck
npm --prefix frontend test
npm --prefix frontend run test:e2e
```

From the repository root, `scripts/browser_lifecycle_smoke.sh` builds and starts a temporary `term-llm serve web --no-auth` process and runs the Playwright chat suite. `scripts/hub_browser_lifecycle_smoke.sh` exercises the bearer-authenticated Hub dashboard and a real proxied chat node, while `scripts/hub_passkey_smoke.sh` exercises passkey setup, login, recovery, and security administration with Chromium virtual authenticators. Run `make frontend-deps` first; standalone smokes fail immediately rather than downloading dependencies implicitly. Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/chromium` to use a system browser locally. CI installs the Playwright-pinned browser.

`scripts/check_frontend_network_policy.sh` fails closed unless ripgrep can scan production sources and enforces transport ownership for `fetch`, browser fetch aliases, XHR, EventSource, WebSocket, and beacon calls. Raw transports are allowed only under `src/api` and in the reviewed WebRTC platform bridge. Run `scripts/measure_ui_payload.sh baseline|final` for the chat graph and `scripts/measure_ui_payload.sh hub` for the standalone Hub entry. The fixed chat baseline and current generated results live in `payload-baseline.json` and `payload-final.json`; `payload-report.md` explains inclusion rules and deltas.

## Build contract

- The existing chat build runs first with `emptyOutDir: true`; `vite.hub.config.ts` then writes into the same directory with `emptyOutDir: false`.
- The chat keeps its relative Vite base and stable `dist/app.js`, `dist/app.css`, `dist/chunks/*`, and `dist/assets/*` outputs.
- The standalone Hub build emits exactly `dist/hub.js` and `dist/hub.css`, with Preact and Signals bundled into `hub.js` and no chat-only dependencies or dynamic chunks.
- Hub assets are intentionally absent from the chat service worker. Only the exact two Hub assets are public before Hub authentication.
- KaTeX and highlight.js remain deterministic chat-only lazy chunks. KaTeX emits only WOFF2 fonts.
- Source maps, manifests, hashes in filenames, and frontend test artifacts are not emitted under `static`.
- `AssetVersion()` covers only chat assets. `HubAssetVersion()` covers exactly `hub.js` and `hub.css`; the canonical Hub module remains unversioned while directly linked Hub CSS uses its scoped version.

Do not edit generated `static/dist` files directly. Change TypeScript/CSS and run `make frontend`; only source and package manifests are committed.
