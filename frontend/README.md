# Embedded Web UI frontends

The chat application and Hub UI are authored in Preact and strict TypeScript under `frontend/src`. The Hub source of truth is `frontend/src/hub/`; its dashboard, bearer login, passkey setup/login/recovery, security administration, behavior, and styling must not be reintroduced into Go templates. Hub HTTP transport is owned by `frontend/src/api/hub-client.ts`.

Vite emits deterministic, minified production files into `internal/serveui/static/dist`. That generated directory is ignored by Git and embedded in the Go binary, so source builds require the frontend build while release binaries remain self-contained.

## Prerequisites

Source builds use Node 24 or newer and npm. The Go build step also writes the deterministic private UI-authoring source archive, which is embedded in release binaries for on-demand extension builder inspection, not downloaded by ordinary browser sessions. The test setup supplies isolated Web Storage implementations for Node 25+, whose process-level `localStorage`/`sessionStorage` globals otherwise depend on runtime flags. CI installs dependencies from `package-lock.json` with `npm ci`.

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

The chat smoke uses HTTP transport only. WebRTC timeout, admission rejection, fallback, and recovery coverage lives in `src/platform/webrtc.test.ts`, using fake timers, abort-aware signaling, and a fake peer/data channel. Do not gate CI on Chromium-to-Go ICE connectivity: host candidates, mDNS, and runner UDP networking made those assertions unreliable even with a loopback signaling relay. The Go peer's admission and protocol tests remain under `internal/webrtc`.

`scripts/check_frontend_network_policy.sh` fails closed unless ripgrep can scan production sources and enforces transport ownership for `fetch`, browser fetch aliases, XHR, EventSource, WebSocket, and beacon calls. Raw transports are allowed only under `src/api` and in the reviewed WebRTC platform bridge. The fixed chat baseline and the last generated results live in `payload-baseline.json` and `payload-final.json`; `payload-report.md` explains inclusion rules and deltas. Those results are a historical record; the measurement script that produced them has been removed.

## Build contract

- The existing chat build runs first with `emptyOutDir: true`; `vite.hub.config.ts` then writes into the same directory with `emptyOutDir: false`.
- The chat keeps its relative Vite base and stable `dist/app.js`, `dist/app.css`, `dist/chunks/*`, and `dist/assets/*` outputs.
- The standalone Hub build emits exactly `dist/hub.js` and `dist/hub.css`, with Preact and Signals bundled into `hub.js` and no chat-only dependencies or dynamic chunks.
- Hub assets are intentionally absent from the chat service worker. Only the exact two Hub assets are public before Hub authentication.
- KaTeX and highlight.js remain deterministic chat-only lazy chunks. KaTeX emits only WOFF2 fonts.
- Source maps, manifests, hashes in filenames, and frontend test artifacts are not emitted under `static`.
- `AssetVersion()` covers only chat assets. `HubAssetVersion()` covers exactly `hub.js` and `hub.css`; the canonical Hub module remains unversioned while directly linked Hub CSS uses its scoped version.

The payload baseline's historical `app-interject.js` entry describes a previous measured asset set; it is not an active chunk name. The current Vite build emits versioned `app.js` and named chunks, including lazy steering actions and queue presentation. Do not rename baseline entries by guessing or relax its budget to mask growth.

Do not edit generated `static/dist` files directly. Change TypeScript/CSS and run `make frontend`; only source and package manifests are committed.

## Storage migration and rollback

Drafts, queued review comments, pending intents, and attention markers are independently keyed
records. New readers retain compatibility reads for the previous aggregate format, but all new
writes use record keys. Rollback must **not** delete or rewrite the new record keys. An older build
may continue reading its aggregate data while a subsequent upgrade resumes additive migration.
Deletion tombstones must also be retained during that compatibility window so deleted legacy
review comments and drafts are not resurrected.

## Accessibility smoke checklist

Run this checklist on a representative desktop and mobile build after interaction changes:

- **NVDA/Firefox or Chrome:** dialog names are announced; Tab and Shift+Tab remain trapped;
  Escape follows the documented neutral-dismiss policy; focus returns to the opener.
- **VoiceOver/Safari:** mobile sidebar, diff drawer, plan sheet, and run center announce their
  labels and expanded state; background chat controls cannot be activated while open.
- **TalkBack/Chrome:** drawer actions remain above the visual keyboard and safe-area inset;
  explicit close controls remain reachable; menus announce menu items and support sequential
  navigation.
- **Media:** closing a lightbox pauses video, clears playback, restores focus, and revokes only
  object URLs owned by the lightbox.
- **Nested surfaces:** opening an approval or media surface above another overlay makes the lower
  surface inert until the top surface closes.
