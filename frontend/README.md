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

From the repository root, `scripts/browser_lifecycle_smoke.sh` builds and starts a temporary `term-llm serve web --no-auth` process and runs the Playwright chat suite. `scripts/hub_browser_lifecycle_smoke.sh` exercises the bearer-authenticated Hub dashboard and a real proxied chat node, while `scripts/hub_passkey_smoke.sh` exercises passkey setup, login, recovery, and security administration with Chromium virtual authenticators. Run `make frontend-deps` first; standalone smokes fail immediately rather than downloading dependencies implicitly. On macOS the specs use an installed Google Chrome (`/Applications` or `~/Applications`), so no `playwright install` is needed; elsewhere, the smoke scripts use a `chromium` on `PATH`. `PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/browser` overrides both. CI installs the Playwright-pinned browser.

The chat smoke uses HTTP transport only. WebRTC timeout, admission rejection, fallback, and recovery coverage lives in `src/platform/webrtc.test.ts`, using fake timers, abort-aware signaling, and a fake peer/data channel. Do not gate CI on Chromium-to-Go ICE connectivity: host candidates, mDNS, and runner UDP networking made those assertions unreliable even with a loopback signaling relay. The Go peer's admission and protocol tests remain under `internal/webrtc`.

`scripts/check_frontend_network_policy.sh` fails closed unless ripgrep can scan production sources and enforces transport ownership for `fetch`, browser fetch aliases, XHR, EventSource, WebSocket, and beacon calls. Raw transports are allowed only under `src/api` and in the reviewed WebRTC platform bridge. The fixed chat baseline and the last generated results live in `payload-baseline.json` and `payload-final.json`; `payload-report.md` explains inclusion rules and deltas. Those results are a historical record; the measurement script that produced them has been removed.

## Build contract

- The existing chat build runs first with `emptyOutDir: true`; `vite.hub.config.ts` then writes into the same directory with `emptyOutDir: false`.
- The chat keeps its relative Vite base. Entry JS/CSS (`dist/app-[hash].js`/`.css`), chunks (`dist/chunks/[name]-[hash].js`), lazy CSS, fonts, and other assets all use content-derived names. CSS grouping is selected before hashing/preload generation, with no post-hash byte rewrites. `dist/asset-manifest.json` is Vite's build manifest; Go reads it to render the entry, classify immutable assets, prewarm chunks, and inject the complete service-worker asset list.
- The standalone Hub build emits exactly `dist/hub.js` and `dist/hub.css`, with Preact and Signals bundled into `hub.js` and no chat-only dependencies or dynamic chunks.
- Hub assets are intentionally absent from the chat service worker. Only the exact two Hub assets are public before Hub authentication.
- KaTeX and highlight.js remain deterministic chat-only lazy chunks. KaTeX emits only WOFF2 fonts.
- Source maps and frontend test artifacts are not emitted under `static`; the generated chat asset manifest is embedded.
- `AssetVersion()` covers only chat assets. `HubAssetVersion()` covers exactly `hub.js` and `hub.css`; both Hub JS and CSS use `?v=<HubAssetVersion>` in the shell and are immutable only on an exact version match.

The payload baseline's historical `app-interject.js` entry describes a previous measured asset set; it is not an active chunk name. The current Vite build emits a hashed entry and named hashed chunks, including lazy steering actions and queue presentation. Do not rename baseline entries by guessing or relax its budget to mask growth.

Do not edit generated `static/dist` files directly. Change TypeScript/CSS and run `make frontend`; only source and package manifests are committed.

## Static caching and deployments

| URL class                                              | HTTP Cache-Control                    | Chat service worker                                             |
| ------------------------------------------------------ | ------------------------------------- | --------------------------------------------------------------- |
| Manifest-listed hashed chat dist assets (any query)    | `public, max-age=31536000, immutable` | Cache-first in scope-specific `term-llm-assets-*`               |
| Icon/web manifest with exact current `?v=`             | `public, max-age=31536000, immutable` | Stale-while-revalidate in version-scoped shell cache            |
| Other embedded non-hashed assets with exact chat `?v=` | `public, max-age=31536000, immutable` | Network-first for scripts/styles/images/fonts; otherwise bypass |
| Other non-hashed assets; missing/mismatched `?v=`      | `no-cache` + ETag                     | Scripts/styles/images/fonts network-first, otherwise bypass     |
| `sw.js` (any query)                                    | `no-cache` + ETag                     | Browser worker update handling                                  |
| Chat navigation/`index.html` (any query)               | `no-cache, no-store, must-revalidate` | Bypass: authoritative network shell                             |
| Hub JS/CSS with exact Hub `?v=`                        | `public, max-age=31536000, immutable` | Bypass                                                          |
| Hub JS/CSS without exact Hub version                   | `no-cache` + ETag                     | Bypass                                                          |
| Hub shell                                              | `no-store`                            | Bypass                                                          |
| APIs, auth, extensions                                 | Existing policies, unchanged          | Bypass                                                          |

Immutable is safe because each chat URL identifies build content, including its
hashed dependency references. All imports/preloads belong to one build graph;
lazy chunks import the same hashed entry URL as the HTML (no query-suffixed
second module identity). The server never substitutes new bytes at an old hashed
URL: missing chunks return 404. Relative paths and `<base>` preserve subpath and
Hub-node proxy mounts. Ordinary HTTP caching avoids per-module conditional
revalidation on cached revisits even without an active service worker, including
WKWebView; the HTML remains authoritative on every navigation.

Service-worker activation drops obsolete version-scoped shell caches but retains
cached hashed files referenced by the current or immediately previous deployment.
Scope-specific metadata records the previous list; repeating activation of the
same build does not advance that window. This supports already-open clients for
one deployment **only for files already cached**. Uncached old chunks can return
404 and lazy UI failures offer a reload. No eager precache, API persistence, or
unbounded historical asset retention is introduced. Failed/auth-redirected
responses are never stored as immutable assets. Version-mismatch hard refresh
clears shell caches and updates the worker, leaving safe content-addressed files
for activation to prune. Cache eviction/storage denial can still require network
fetches; this is not an offline application.

## Startup caches (fast reopen)

Chat startup has three milestones, recorded as `performance.mark('term-llm:<name>')`
and in `AppStore.startupMetrics` (timings and counters only, never transcript text):

1. `firstUsefulPaint` — the shell shows a restored last-known workspace, or the fresh
   sidebar with the selected conversation hydrating.
2. `authoritative` — the selected conversation (or new chat) is confirmed by the server.
3. `actionsReady` — `startupDone`; lifecycle, server events and sending are enabled.

Model discovery, skills, branches and Hub agent health never block 1 or 2. The event
feed gets a 150 ms head start; if its cursor arrives later, one catch-up sidebar/status
reconciliation covers the gap. Background 401/403s fail the running bootstrap or
return the user to the credential gate; they are never swallowed.

Persistent caches live in IndexedDB (`term_llm_ui_cache`, `platform/persistent-cache.ts`)
and are display hints only. Every record is reconciled by the normal authoritative
reads; authorization, liveness, pending approvals/questions, and provider
availability are always server-decided.

| Record (per scope)                  | Bound                                              | Freshness                                                                                      |
| ----------------------------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Chat sidebar/projects (first page)  | 1 record, ≤ 800 KB                                 | shown ≤ 14 days old; replaced wholesale by the first server snapshot                           |
| Recently viewed conversations       | LRU of 8, ≤ 700 KB each, ≤ 4 MB total, ≤ 80 msgs   | shown ≤ 14 days old; hydration replaces the bodies (revision-checked)                          |
| Provider list, model catalogs       | ≤ 12 records, ≤ 2 MB                               | shown ≤ 7 days old; models refetched at startup when > 10 min old; provider change invalidates |
| Hub nodes / attention / delegations | 3 records, ≤ 128 KiB each; ≤ 200 nodes, ≤ 50 items | shown ≤ 7 days old; each section replaced as its request lands                                 |

**Scope and privacy.** Keys combine origin, UI prefix, Hub node id and a server-injected
opaque `TERM_LLM_CACHE_SCOPE` (chat: SHA-256 of the per-database random
`store_instance_id` and auth mode; Hub: its random passkey user handle or state identity).
Credentials are never key material. Without a scope, private persistence is disabled.
Cached private content is displayed before an API response only when the HTML
navigation itself required authorization (passkey, Hub-proxied nodes, Hub dashboard) or
the server runs without auth. Bearer-token chat servers serve public HTML, so their
cache waits for the first authenticated response. Any 401/403 purges the scope; a
token change purges it; Hub logout deletes the whole database; Settings → Connection
offers "Clear cached data on this device".

At rest, records are plain IndexedDB data in the browser profile. That matches the
existing boundary: bearer mode already keeps the server token in `localStorage` of the
same profile, and passkey sessions live in its cookie jar, so anyone with profile
access already has API access. The injected scope is an opaque hash and grants
nothing by itself. The Hub scope is account-scoped (passkey user handle, or a random
sidecar identity); Hub content comes from live nodes, so a replaced node list is
reconciled on the first nodes response and removed nodes' summaries are purged.

**What is never cached:** tokens/cookies, auth or permission decisions, approvals and
ask-user prompts, live stream state (`activeRun`, `activeResponseId`), history paging
anchors, the installed-bodies revision, attachment bytes, `data:`/`blob:` URLs, and
signed URLs. Restored rows cannot resume a stream, acknowledge attention, or page older
history until the server confirms them. Routed launches only restore the exact routed
conversation; `?new=1` and active drafts restore as a new chat. A conversation that no
longer resolves (404/410 or `selected_session: null`) is forgotten locally.

**Schema changes.** Bump `WORKSPACE_CACHE_SCHEMA` / `HUB_CACHE_SCHEMA` when a persisted
shape changes; mismatched records are ignored and replaced. The IndexedDB version
upgrade drops the store, since every record is disposable.

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
