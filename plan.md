# Preact Web UI Rewrite Plan

## Objective

Replace the term-llm chat Web UI under `internal/serveui/static` with a fully authored Preact + TypeScript application, built and minified by Vite, while preserving current behaviour, server contracts, deployment properties, base-path support, Hub/WebRTC integration, service-worker behaviour, and self-contained Go binaries.

This is a complete rewrite of the chat application UI, not an indefinite hybrid migration. Temporary compatibility adapters are allowed while implementing, but must not remain in the final runtime. The separate Hub templates under `cmd/templates` and the docs site are outside this rewrite unless a compatibility change is required.

## Non-negotiable completion criteria

- No production runtime `window.TermLLMApp` application namespace. A narrowly gated `window.__TERM_LLM_TEST__` bridge is permitted only when enabled explicitly for browser tests, must expose no credentials, and must be absent during normal startup.
- No ordered list of first-party application `<script>` tags.
- No authored application modules implemented as global IIFEs.
- No direct DOM ownership outside narrowly justified adapters for browser/platform libraries.
- All chat UI surfaces are rendered and owned by Preact, including the shell, sidebar, transcript, composer, settings, pickers, modals, plans, side questions, approvals, attachments, session administration, branching, diffs/comments/queue, worktrees, skills, goals, MCP status, path notes, interjections, and response effects.
- Production JavaScript and CSS are bundled and minified.
- `go build` and normal binary users do not require Node at runtime or build time; generated frontend assets are committed and embedded.
- Existing URL/API contracts and Go-injected globals/configuration remain compatible unless deliberately replaced on both sides.
- Existing behaviour tests are either retained against the built output or replaced by equivalent/better TypeScript/component/browser tests. Behaviour must not be silently dropped because a legacy test was awkward.
- Current WebRTC and Hub-aware `/chat` flows continue working.
- A before/after payload report includes raw, gzip, and Brotli sizes for the actual first-party initial-load HTML, CSS, and JavaScript, plus request count. Vendor payloads must be reported separately so bundling does not disguise movement between categories.

## Architecture

### Toolchain

- Preact (native APIs, no `preact/compat` unless a specific dependency requires it)
- TypeScript in strict mode
- Vite production build with minification enabled
- Vitest for domain/store/component tests
- Playwright for a small set of browser-critical flows
- npm lockfile for reproducible builds

Vite must emit deterministic filenames, for example:

- `internal/serveui/static/dist/app.js`
- `internal/serveui/static/dist/app.css`
- deterministic lazy chunks under `internal/serveui/static/dist/chunks/`

The build contract is pinned: `base: './'`, `build.manifest: false`, `build.sourcemap: false`, `emptyOutDir: true`, `assetsInlineLimit: 0`, explicit deterministic `entryFileNames`/`chunkFileNames`/`assetFileNames`, and no test output under `internal/serveui/static`. Use `//go:embed all:static/dist` so generated subdirectories are embedded deliberately. Vitest and Playwright artifacts belong under `frontend/` or a temporary directory.

Use ES modules and pin `.js`/`.mjs` to `text/javascript; charset=utf-8` and `.css` to `text/css; charset=utf-8` in the Go UI asset handler before switching the page to `type="module"`; cover this with Go tests. Unknown `*.js` and `*.css` UI paths must return a real asset error rather than the HTML SPA shell.

Go's existing `AssetVersion()` query parameter remains the cache-busting mechanism. Do not introduce hashed filenames. Preserve the exact Go/Hub integration contract: relative asset URLs, the current `<base>` insertion point, and the `window.TERM_LLM_UI_PREFIX=<json>` bootstrap shape used by `hubRebaseUIPrefix`. Parse injected globals only during deferred app bootstrap, after Hub context injection. Any dynamic import must be tested under direct `/chat` and a Hub node mount because it resolves relative to the importing module URL.

Use these npm scripts as the public workflow: `build`, `typecheck`, `test`, `test:e2e`, and `check:generated`. CI uses pinned Node plus `npm ci`; local and release runtime users do not need Node because generated assets are committed.

### Source layout

```text
frontend/
  src/
    app/
    api/
    components/
    domain/
    platform/
    stores/
    styles/
    test/
  package.json
  package-lock.json
  tsconfig.json
  vite.config.ts
```

- `api`: typed transport and endpoint modules; no DOM access.
- `domain`: session/message/stream/diff models and pure transitions; no DOM or HTTP access.
- `stores`: feature-oriented state and actions. Components do not mutate ambient shared objects.
- `components`: Preact-owned markup and event handling.
- `platform`: local storage, clipboard, notifications, service worker, media recorder, WebRTC bridge, observers, scrolling primitives.
- `app`: composition, bootstrapping, injected config parsing and top-level error handling.

### State model

Avoid replacing one giant mutable object with one giant signal object.

Use feature stores with explicit action methods and computed state. Preact Signals may back those stores where granular updates are useful, but mutations must remain encapsulated. Model response/session lifecycles with typed discriminated unions and explicit transitions rather than independent booleans and generation counters wherever practical.

Keep server state, persisted preferences, transient UI state, and active-run state distinct.

### Rendering, CSS, vendors and performance

- Import the existing `app.css` substantially verbatim for the first complete rewrite. Preserve required IDs/classes where current selectors constrain markup; do not combine the architecture rewrite with a visual redesign. Preserve startup theme metadata, safe-area rules, reduced-motion behavior and responsive breakpoints.
- Bundle `marked` and DOMPurify from pinned npm dependencies. Load KaTeX and highlight.js as deterministic lazy Vite chunks so the initial payload remains comparable to their current lazy-loading behavior. Update or retire `scripts/update-web-dependencies` coherently; never count moved vendor bytes as an application saving.
- Preact owns the full DOM below the application mount at completion. During implementation there may be exactly one compatibility boundary, `frontend/src/app/legacy-bridge.ts`; it must be deleted at final cutover. The final-DOM-ownership criteria apply to the finished tree, not intermediate commits.
- Transcript rows use stable keys and preserve current scroll anchoring/windowing semantics.
- Streaming updates must avoid reparsing/rerendering the complete transcript on every chunk.
- Markdown parsing, sanitisation, syntax highlighting, KaTeX, media handling, diff rendering, clipboard behaviour, and asset URL rebasing retain current security and behavioural guarantees.
- Keep DOMPurify or an equivalently reviewed sanitizer. Do not weaken current sanitisation during conversion.
- Preserve accessibility labels, focus restoration, keyboard interaction, reduced-motion behaviour, responsive layouts, and safe-area handling.

## Execution sequence

### 1. Establish a trustworthy baseline and coverage disposition

Before replacing assets:

1. Record branch/base SHA and repository status.
2. Run the current focused Go and Node frontend tests, including their current sharding environment.
3. Run `scripts/browser_lifecycle_smoke.sh` and the Hub passkey/recovery smoke path where the environment permits it.
4. Create `frontend/test-parity.md` before deleting any asset. Inventory every legacy production JS file and every Go/Node test that names, source-greps, size-ratchets, or loads it. For each test record the behaviour/policy it protects and one disposition: retain against bundle, rewrite as a Go bundle/HTTP policy test, port to Vitest, port to Playwright, or retire with a written reason.
5. Replace the current line-count ratchets with durable raw+gzip bundle-size budget tests over production `dist/app.js` and `dist/app.css`. Replace the source-grep prohibition on raw `fetch(` with a source-level lint/test rule allowing transport only under `frontend/src/api/**` and the narrowly scoped WebRTC platform bridge.
6. Decide each existing browser-lifecycle scenario explicitly. Prefer UI-driven Playwright; use the explicitly enabled `window.__TERM_LLM_TEST__` bridge only for lifecycle behavior that cannot be driven faithfully through public UI. Never leave the bridge enabled in production startup.
7. Produce a baseline asset inventory from the rendered production page:
   - first-party HTML/JS/CSS request count
   - third-party/vendor request count
   - raw bytes
   - gzip bytes using the server-equivalent gzip level 6
   - hypothetical Brotli bytes at a fixed documented quality (Brotli is not currently served)
   - service-worker precache request/byte totals, separately from cold navigation
8. Produce a second control baseline by minifying the legacy authored JS/CSS with the same minifier settings used by the new build, then applying the same gzip/Brotli measurement. This separates toolchain/minification gains from architecture/framework cost.
9. Save the exact measurement script as `scripts/measure_ui_payload.sh` and raw baseline data under `frontend/payload-baseline.json` so final numbers are reproducible.
10. Capture representative desktop and mobile screenshots for regression comparison if the existing harness permits it without requiring private credentials.

Measurement inclusion rules must be fixed before implementation. Count cold initial navigation, lazy KaTeX/highlight assets separately, and the service-worker shell precache separately. Measure comparable production artifacts with the same optional WebRTC setting, route/base path, compression settings and vendor inclusion rules. Do not use `AssetVersion()` as a payload proxy.

### 2. Add frontend build and embedding

1. Create the TypeScript/Vite project using the pinned contract above and add `frontend/node_modules/` plus test artifacts to `.gitignore`.
2. Configure deterministic, minified production output and import the existing CSS without redesigning it.
3. Add the reproducible npm scripts and a CI job that runs pinned Node, `npm ci`, typecheck, tests, build, and `git diff --exit-code internal/serveui/static/dist`. `.goreleaser.yml` remains unchanged because it builds committed generated assets.
4. Update Go embedding, index rendering/versioning, explicit module MIME handling, service-worker asset inventory, Hub rebasing assumptions, and HTTP tests for the new bundle.
5. Add a Go test ensuring unknown `*.js`/`*.css` paths do not return the SPA shell. Preserve the current `RenderServiceWorker` cache-key assumption unless deliberately updated and tested.
6. Keep generated assets committed so plain `go build ./...` remains sufficient.
7. Add repository documentation for rebuilding/testing frontend assets.

For each migrated slice, definition of done is: legacy file deleted from disk; removed from `embed.go`; removed from `RenderIndexHTML` version replacements; removed from `RenderServiceWorker` replacements; removed from `index.html` preload/script tags; removed from `sw.js` shell assets; and every associated Go/Node test disposition completed. Shared legacy state may require files to be deleted together at cutover; do not fake slice completion by leaving two runtime owners.

### 3. Rebuild domain and platform foundations

Port and type the existing behaviour before composing the final UI:

- all injected globals and defaults: UI prefix/version, sidebar categories, agent name/list, UI title, location/worktree flags, Hub context, VAPID key, WebRTC enabled/signaling URL
- all scoped storage keys and migrations, including the special token scoping rule, per-session intent keys, optimistic transcripts, and cross-tab pending-intent catch-up
- typed API/error/network recovery layer, including coordinated startup, `pageshow`, visibility and reconnect recovery
- session routing through push/replace/popstate
- session/message conversion
- active response and streaming event reduction, with an explicit parity inventory of every current `response.*` SSE event before porting
- transcript windowing
- markdown/sanitisation/decoration
- runtime provider/model/effort selection
- attachments and media/voice platform integration
- notification permission state, service-worker registration, web push/VAPID subscription and platform notification behavior
- clipboard API plus `execCommand` fallback
- WebRTC transport/fallback integration
- widget state/navigation and Hub asset rebasing
- diff comments/queue/scopes/context domain logic

Prefer pure functions and focused tests. Keep transport fallbacks and Hub asset rebasing explicit.

### 4. Rebuild every UI surface in Preact

Implement and verify feature slices in an order that keeps the application runnable:

1. App shell, startup/auth/error boundary and responsive layout
2. Header, runtime selectors and settings
3. Sidebar, project/session grouping/search and session administration
4. Composer, slash/mention completion, attachments, voice and send/interject controls
5. Transcript, streaming markdown, tool/guardian/plan/usage rendering and response effects
6. Ask-user, approval, side-question and modal/lightbox surfaces
7. Branching, path notes, goal/location, skills and MCP surfaces
8. Diff sidebar, scopes, comments, queue and worktrees
9. Widgets/Hub navigation and optional WebRTC integration

Each slice must remove the corresponding legacy runtime implementation rather than leaving two owners.

### 5. Test conversion, moving-baseline control and parity audit

- Port legacy behavioural tests into Vitest/component tests or keep them exercising the generated bundle until superseded. JavaScript coverage no longer depends on `go test` finding Node ad hoc: the dedicated frontend CI job is mandatory.
- Own shared completion fixtures under `frontend/src/test/` or document any retained cross-tree fixture explicitly.
- Create a parity checklist mapping every legacy production module and significant test group to its new owner.
- Search the complete repository for old asset names, `TermLLMApp`, stale selectors, and bypasses around the shared network/auth layer.
- Test both ordinary direct `/chat` mode and Hub-aware mounted mode, including lazy chunk loading after Hub HTML rewriting.
- Test WebRTC enabled and disabled.
- Test authenticated and auth-required startup.
- Test desktop and mobile layouts.
- Test streaming, cancellation, continuation, interjection, reconnection and session switching races.
- Test diff comments/queue and worktree flows.
- Test service worker upgrade/cache behaviour.
- Before final review, fetch/rebase onto current upstream main and inspect `git log --oneline $(git merge-base HEAD upstream/main)..upstream/main -- internal/serveui/static internal/serveui`. Record every intervening frontend behavioural change in the parity checklist and mirror it. Ask for a short merge/cutover freeze if the directory continues changing during final verification.

Do not delete a legacy test merely because its setup no longer fits. First identify the protected behaviour and replace its coverage. The PR reports the final merge-base used, not merely the original branch point.

### 6. Optimise and measure production payload

- Confirm Vite minification and dead-code elimination on production output.
- Inspect bundle composition and remove accidental duplicate libraries or compatibility layers.
- Lazy-load genuinely secondary heavy surfaces only where it does not compromise offline/update behaviour or create fragile chunk bookkeeping.
- Re-run the exact baseline measurement script against final production output and the same lazy-asset scenarios.
- Report four comparable columns: legacy delivered assets, legacy assets minified by the new toolchain, final Preact assets, and delta from each relevant baseline.
- Report first-party and vendor payloads separately and combined.
- Include raw, actual gzip-level-6 and hypothetical fixed-quality Brotli deltas in bytes and percentages, plus cold-navigation and service-worker-precache request-count deltas.
- Do not claim an architectural improvement if bytes merely moved from separate vendor files into `app.js`, or if the gain is explained by minifying previously unminified legacy code.

### 7. Final verification

At minimum:

- frontend typecheck
- frontend unit/component tests
- production frontend build and generated-assets freshness check
- focused `go test ./internal/serveui ./cmd`
- `go test ./...`
- `go vet ./...`
- `gofmt -l` on changed Go files must be empty
- `go build ./...`
- `git diff --check`
- Playwright/browser smoke covering load, send/stream, cancellation, session switching, settings, sidebar, mobile viewport, diff UI, Hub/base-path behaviour and WebRTC where the environment supports it

Inspect generated files, source maps, licences and lockfile deliberately. Do not ship source maps or development-only code unless explicitly intended.

## Review gates

1. Opus reviews this plan before implementation; incorporate material corrections.
2. A developer agent performs the implementation in the prepared isolated worktree.
3. Opus reviews the complete implementation and test/payload evidence.
4. Address blocking and high-value review findings, rerun affected verification, then publish the PR.

## PR report

The PR description and final report must include:

- architecture summary
- legacy-to-new parity map
- generated asset/build workflow
- tests and browser scenarios run
- known limitations or unverified environment-specific behaviour
- exact base/head SHAs
- payload table with baseline, final, byte delta and percentage for raw/gzip/Brotli
- initial request-count comparison
- screenshots or concise visual parity notes where useful

## Scope discipline

This is already a large rewrite. Do not redesign the visual language, alter backend APIs gratuitously, rewrite the separate Hub UI, or add a general component library. Preserve the product while replacing the implementation. Any behaviour or visual change must be identified and justified in the PR rather than smuggled into the rewrite.
