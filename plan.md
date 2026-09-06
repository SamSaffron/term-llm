# Make UI Mine: directory-based web extensions

## Implementation record

The first working implementation now includes directory/config/flag-controlled loading, immutable in-memory asset snapshots (not a publication registry), the settings picker, sticky safe mode, the built-in extension builder, source inspection/fallback/cache, release-source publishing, examples, and documentation at `docs-site/content/guides/extensions.md`.

The clean-room exercise installs Dracula plus a JavaScript clock, enables them through extension-local settings, sends an extension-builder session request, verifies snapshots/cache behavior, deliberately breaks the layout, and repairs it through safe mode on desktop and mobile. It also exposed and fixed startup-root color inheritance and desktop modal scrolling. A separate scripted-provider test drives the real engine through source read → file writes → local settings edit → ui_activate_extensions. This is deterministic infrastructure/UX testing, not a live-model creativity evaluation.

Implementation choices: `ui_get_source` handles stock/compiled references; installed extensions use the normal permission-aware file tools with paths from `ui_extensions`. Cached release archives are decompressed lazily in memory, with explicit extraction into a new approved workspace directory. Cross-node exact-source retrieval is not yet implemented: a browser/source-host mismatch is labeled approximate rather than blocking useful local/published references. WebRTC-only module loading is unsupported; direct and authenticated Hub HTTP loading are tested. GitHub source assets become available when the updated release workflow runs.

## Goal and model

Let a user talk to a specialist agent that reshapes the term-llm chat web interface with CSS + JavaScript. Like widgets, extensions live in a directory. The web process loads an ordered enabled list from configuration or boot flags. A minimal UI lets the user select that list, and the agent is allowed to edit configuration to enable what it builds.

Use **extensions** as the proposed name: a theme is a CSS-only extension, while JavaScript can change behavior. Each subdirectory is one extension bundle. No separate theme system or general plugin framework is required.

The demo is: “Make this feel like a reading app, compact the sidebar, and add a clock.” The agent inspects current source, writes an extension, enables it in config, calls `ui_activate_extensions`, and idle connected HTTP browsers reload automatically. No mandatory browser-local Try/Keep approval or immutable publication workflow.

Intentional trade-off: trusted customization can break the interface. The recovery mechanism is a stock-UI safe-mode URL, plus disabling extensions in config/boot flags. This replaces the earlier plan's browser-local saved/preview selection and revision-publishing model throughout.

## Current architectural grounding

- Chat is Preact + TypeScript + Signals under `frontend/src`; `frontend/src/main.tsx` bootstraps it. Vite assets are embedded in the Go binary. Extensions must work with release binaries without rebuilding or editing generated assets.
- `internal/widgets/manifest.go`, `cmd/serve_widgets.go`, and `resolveWidgetsDir` in `cmd/serve.go` provide the directory discovery, config/flag precedence, and authenticated status/reload route patterns. Reuse these conventions, not the widget subprocess/proxy runtime.
- Widget configuration already uses `serve.widgets_dir` and `--widgets-dir`. New extension fields must be wired into `internal/config/config.go`, its schema, CLI flags, and tests rather than existing only in the frontend.
- `internal/agents/builtin/widget-builder/` provides the specialist agent pattern. Existing file tools support extension editing with a narrow host-scoped capability for the verified built-in web agent.
- Existing agent-aware web sessions/runtimes can run the extension builder as a separate ordinary conversation. Do not repurpose the deliberately tool-free side-question engine.
- Existing CSS tokens are useful, but the UI is not completely tokenized. Direct CSS/DOM work is allowed; complete tokenization is not a prerequisite.
- Preact owns the DOM. Provide helpful mounting points and guidance, not a promise that private DOM structure is stable.
- First-party transport belongs under `frontend/src/api` and must preserve authentication, base paths, and Hub routing. This source convention is not a sandbox for installed JavaScript.

## Directory, manifest, and enabled list

Proposed default, resolved through existing XDG/application-config conventions:

```text
~/.config/term-llm/extensions/
  reading-room/
    extension.yaml
    style.css
    main.js
    assets/
  header-clock/
    extension.yaml
    main.js
```

Proposed manifest:

```yaml
title: Reading room
description: Warm reading layout with compact navigation
format_version: 1
css: style.css
js: main.js
```

Directory name is the extension ID. CSS and JS are independently optional, but at least one entry is required. Entries and asset references are relative to the extension directory. Fonts/images/local module imports are supported. Validate IDs, manifest version, containment, symlinks, file types, and size limits. Serve only validated extension assets, never arbitrary config-directory files.

Proposed config (names are new, not existing options):

```yaml
serve:
  extensions_dir: /path/to/extensions  # optional; default follows XDG config root
  extensions:
    - reading-room
    - header-clock
```

Proposed boot flags:

```sh
term-llm serve web --extensions reading-room,header-clock
term-llm serve web --extensions-dir ./extensions --extensions reading-room
term-llm serve web --disable-extensions
```

Rules:

- Scan the directory for available extensions. Discovery alone does not enable every entry; the enabled list decides what loads. The agent may add its extension to that list as part of fulfilling the request.
- Multiple extensions can load together. Preserve list order: append CSS in that order, and activate JS sequentially in that order after the host mounts. Later CSS wins according to the normal cascade; do not build dependency/conflict resolution in v1.
- Explicit CLI directory/list values override their config counterparts. An explicitly empty list means no extensions, not “fall back to config.” Reject duplicate enabled IDs or normalize them deterministically with a visible diagnostic.
- `--disable-extensions` wins over config/list settings. It is a process-wide kill switch; safe mode is the browser-local escape hatch.
- Report missing/invalid enabled entries individually; keep stock UI and other valid entries usable. A missing directory is an empty discovery result.
- Configuration affects browsers served by this web process, not only the tab that edited it. Clearly show this scope in settings and agent instructions.

## Configuration and reload workflow

The extension directory and enabled config list are the source of truth. No required publish tool, immutable revision registry, per-browser activation database, or mandatory Try/Keep cycle.

Agent workflow:

1. Inspect the effective extension directory, persisted list, CLI overrides, running frontend version, and relevant stock/extension source.
2. Create or edit extension files using existing tools. The verified built-in web agent has automatic file-tool access only to the extension root, not its parent, main config or shell.
3. Add/remove/reorder its ID in the extension root’s `extensions.yaml` (`enabled`) as needed. Legacy `serve.extensions` is a read-only fallback until that file exists. Preserve comments and other enabled extensions; do not require an extra browser approval to make this take effect.
4. Invoke `ui_activate_extensions` to rescan files/settings and request automatic browser activation. CLI overrides continue to win.
5. Browsers poll the authenticated status endpoint and reload only for an explicit activated generation, after responses finish and drafts/attachments and dialogs are clear. Safe mode never auto-activates. No manual activation step is required.

Add status/reload routes following widget conventions. Reload is explicit in v1; file watching is optional later. Do not reload arbitrary provider/auth/server settings as a side effect. For directory changes, build the new registry before swapping it into service. Reject malformed config updates without discarding the previous valid registry, and return useful diagnostics.

The minimal settings UI may write the enabled list through a server endpoint that updates only `extensions/extensions.yaml`, leaving the main config untouched, preserving comments and detecting stale concurrent edits. Reuse existing config update utilities where available. Report permission/read-only errors accurately; do not silently save to localStorage instead.

When CLI flags override config, expose the effective value and its source. The picker should show the overridden selection as process-controlled rather than pretend a config edit can change it. The agent may edit persisted defaults for the next boot, but must explain that changing a boot override requires restarting with different flags. No endpoint can bypass `--disable-extensions`.

On successful reload, existing tabs do not need to hot-execute new JavaScript. New page loads use the effective list; explicit activation requests automatically reload idle HTTP tabs. Plain rescans never trigger automatic reload. This is a lifecycle choice, not a browser-local authorization gate.

## Minimal selection UI

Add an **Interface / Extensions** section in Settings:

- List discovered extensions with name, description, enabled checkbox, and load/validation errors.
- Allow ordering the enabled set and **Save and reload**. Persist the set in process configuration, not browser storage.
- **Disable all** writes an empty enabled list when config controls selection.
- **Make UI Mine / Edit with agent** starts or resumes the specialist session with the relevant extension IDs and effective process context.
- Show config-versus-CLI source, directory, effective enabled set, and what this browser actually loaded. Distinguish pending changes from the current document's loaded set.
- Show shared-process impact and a safe-mode link. No marketplace or revision picker in v1.
- In safe mode, the same stock UI can inspect/edit config and repair extensions without loading them. Saving config must not implicitly exit safe mode; provide an explicit Exit safe mode and reload action.

## Trusted code and recovery

Extensions are trusted application-level CSS/JS. They can read displayed conversations and act with the browser's authenticated access. The user or their agent may intentionally install and enable them through config. Do not impose an additional per-browser consent gate or advertise a sandbox.

Safe mode handles accidental UI breakage. It cannot undo malicious requests, disclosure, storage mutation, or persistent browser compromise. State this plainly in documentation. Standard config/workspace/server authorization still applies; an extension route must not become an arbitrary filesystem-read API.

First scope is chat only, not Hub login/security pages or shared/exported transcripts. No marketplace, remote-code installer, or supported service-worker registration API in v1. These are scope limits, not enforceable restrictions on trusted JS.

Proposed recovery URL: `?safe-mode=1` under the current app base path.

- Check safe mode at the earliest chat bootstrap point, before requesting any extension assets. Bypass the entire effective list regardless of config or flags.
- Remember safe mode in scoped `sessionStorage` until explicitly exited. `platform/routing.ts` currently drops query parameters during session navigation; handle that. If storage fails, preserve the recovery parameter on navigation rather than failing open.
- Safe mode does not rewrite process config or disable extensions for other users. Stock controls may separately change config if authorized.
- Provide the safe-mode URL before first extension use and document `--disable-extensions` as the process-level alternative.
- A full document reload is the reliable reset boundary. Removing a script element or invoking a cleanup hook cannot undo arbitrary JS changes. Start with reload-based updates; hot replacement is optional later.

## Asset loading and file changes

Use authenticated app-relative extension routes compatible with direct base paths and Hub proxies. Keep supported assets separate from config/manifests that are not intended as public resources. Validate MIME types, CSP compatibility, and module/stylesheet authentication, not just JSON endpoint authentication. Dynamic imports cannot attach the API client's arbitrary auth headers; verify the supported same-origin auth path and explicitly handle unsupported transports.

`internal/serveui/static/sw.js` currently caches same-scope script/style/image/font requests beyond `SHELL_ASSETS`. Add an explicit extension-route exclusion before `respondWith`, with upgrade/cache-cleanup tests. Extension files must not be served out of the shell cache after edits or disabling. Account for an older controlling worker on first use.

Use a reload generation/content fingerprint in extension URLs and a deliberate no-stale-cache policy. Relative imports and CSS assets must retain the extension route/generation prefix. Files remain editable in the directory: there is no public immutable-release model. Detect content changes against the scanned generation and return reload-needed rather than silently mixing new files into an old generation. Prefer completing writes before explicit reload; do not silently overwrite user files.

This fingerprint is cache/lifecycle metadata, not a user-selectable revision or historical source archive. A stale browser may report a fingerprint for which current source no longer exists; report that mismatch and ask it to reload rather than claiming current files are the old bytes.

Load JS as ES modules via dynamic import and call optional `activate(ui)` after app mount. Catch fetch/parse/activation errors and report per-extension diagnostics where possible. Side effects before a throw cannot be cleanly reverted; infinite loops cannot be recovered in-process. Safe-mode reload remains the escape hatch. A brief stock-style flash is acceptable in v1.

## Source access for the agent

The agent needs both stock implementation source and the installed extensions/configuration it is changing. Documentation alone is insufficient.

### Stock source in release binaries

`internal/serveui/embed.go` already embeds generated HTML/CSS/JS. `frontend/vite.config.ts` minifies JS and sets `sourcemap: false`; original TSX is not currently embedded. Extracting compiled files is possible today, but pretty-printing does not recover original source.

During frontend build, generate a deterministic compressed archive of the relevant first-party chat CSS/TS/TSX and authoring documentation. Embed it separately from public `static/dist`, tied to the generated asset version and its own source digest. Exclude tests, node_modules, unrelated Hub pages, private build paths, and runtime configuration. Do not enable public source maps as a workaround. Measure binary overhead and test build/archive consistency.

### Best-effort source fallback

Source inspection must be useful even when exact readable source is unavailable. Do not block customization just because a build is old, local, or missing its source archive. Resolve references in this order:

1. Exact readable source matching the browser's frontend build, from the owning binary or a matching published source archive.
2. If exact source is unavailable, use **the most recent published release source for development/unversioned builds**, or **the closest published version lower than the running version for release builds**. Compare semantic versions, not tag strings or publication timestamps; use the most recent stable release for the development fallback.
3. If there is no suitable published source, or retrieval fails/offline access prevents it, expose whatever compiled HTML/CSS/JS/chunks are available from the UI-serving binary. Minified code is still useful. Optional formatting is an inspection aid, not recovery of original source.

Keep actual compiled assets available alongside approximate readable source so the agent can check selectors and behavior against the running implementation. If the host itself differs from the browser's loaded build, label that mismatch too; never claim approximate source or different-build compiled assets are exact. Missing readable source should produce a warning and usable fallback, not just an error asking the user to refresh.

Every result includes requested UI version/asset hash, resolved source version/digest, provenance (embedded or published), match quality (exact, approximate, or compiled-only), and the reason for fallback. Instruct the agent to treat older source as guidance and verify assumptions against available current assets. Return a clear unavailable result only when no usable source or compiled assets can be obtained.

Published fallback retrieval is on demand from a defined project release source, not an arbitrary tool-supplied URL. Reuse cached archives, verify published checksums where available, and apply size/path/extraction limits. Network failures fall through to local compiled assets rather than preventing work. The release build/publish workflow must make versioned source archives discoverable alongside release identity; do not require private build paths or repository credentials.

### GitHub download contract and local cache

Use the existing updater's repository identity (`samsaffron/term-llm` in `internal/update/check.go`). These source artifacts are **new release outputs**, not files already published today.

- Discover release tags/assets through `https://api.github.com/repos/samsaffron/term-llm/releases?per_page=100`, following pagination with a bounded request budget. Ignore drafts/prereleases; choose candidates using semantic versions according to the fallback rules above. Reuse cached discovery metadata. If a bounded search cannot find a candidate, return the available compiled fallback rather than claiming no older release exists.
- For a chosen release tag such as `vX.Y.Z`, download the platform-independent archive at `https://github.com/samsaffron/term-llm/releases/download/vX.Y.Z/term-llm-ui-source.tar.gz`.
- Download its checksum list from `https://github.com/samsaffron/term-llm/releases/download/vX.Y.Z/checksums.txt`. Require the archive's SHA-256 entry for these new curated artifacts and validate before extraction. This provides integrity against accidental corruption, not an independent signature/trust root.
- The archive contains `source-manifest.json` (format version, release/build identity, chat asset version, source digest, and file inventory) plus readable paths such as `frontend/src/components/Sidebar.tsx` and `frontend/src/styles/base/tokens.css` and the bundled authoring reference. Paths are archive-relative and contain no machine-specific checkout prefix. Only claim an exact match when the asset identity matches, not merely the release tag.
- Extend `.github/workflows/release.yml` and `.goreleaser.yml` to generate this archive from the same frontend build inputs, attach it once per release, and include it in `checksums.txt`. It is not an OS/architecture-specific binary archive.
- Older releases may lack the new asset. Skip missing assets and continue to the next eligible published source release within the lookup budget; if none can be obtained, use compiled assets. Do not silently fetch `main` or GitHub's whole-repository zip as if it were the curated archive.

Cache under `$XDG_CACHE_HOME/term-llm/ui-source/`, falling back to `~/.cache/term-llm/ui-source/`, matching the existing cache-home convention in `internal/cache/models.go`:

```text
ui-source/
  releases.json                         # release/asset metadata, ETag, fetched_at
  releases/
    vX.Y.Z/
      metadata.json                     # provenance, validated SHA-256, identities
      <sha256>/
        term-llm-ui-source.tar.gz
        source/                         # lazily extracted reference tree
```

Use a 24-hour freshness window for release discovery, conditional requests when possible, and stale cached metadata on network failure/rate limiting. Verified content-addressed archives do not expire just because metadata ages. Check the cache before downloading; concurrent requests share a lock, download to temporary files, validate checksum and extraction limits, then rename atomically. Never reuse a partial/corrupt archive or overwrite a modified extracted tree silently. The archive/source tree is reference data; editable extensions remain in `extensions/` and an agent may explicitly materialize a working reference copy into its granted workspace.

Treat the cache as disposable: deleting `ui-source/` forces on-demand retrieval next time. No background downloads or automatic garbage collector in v1. If cache storage is unavailable, report it and still offer embedded/compiled source rather than blocking customization. Network calls use timeouts/size limits, fixed project URLs (including normal GitHub asset redirects), and no model-supplied host or credentials. Public release retrieval needs no GitHub account; authentication/rate-limit failure degrades to cached or compiled references.

### Proposed `ui_get_source` tool

Inventory-first, bounded reads, and optional extraction into an authorized reference directory:

```jsonl
{"target":"stock","operation":"list"}
{"target":"stock","operation":"read","path":"frontend/src/components/Sidebar.tsx","start_line":1,"end_line":180}
{"target":"extension","id":"reading-room","operation":"list"}
{"target":"stock","operation":"extract"}
```

- Stock target exposes readable source and exact compiled assets/chunks. Export the static HTML template, never rendered HTML with injected credentials/configuration.
- Extension target exposes installed manifest/CSS/JS/assets and their current fingerprints; ordinary file tools can then edit the actual directory. There is no separate publication workspace to synchronize.
- Context reports the directory, effective ordered list, config/flag provenance, browser-loaded set/fingerprints, safe mode, and frontend asset version. Refresh context per extension builder request. A tab may still be running the previous set after a config reload.
- Compare the browser's UI version with the source host. Through Hub, the agent node may not own the frontend binary; try the matching UI host first, then follow the best-effort fallback policy. Label approximate references explicitly rather than refusing useful older source.
- Keep config inspection narrowly scoped to extension settings; do not expose the full config with credentials as source context.
- Extraction validates paths and avoids overwriting edited references. Source inspection is not permission to edit generated deployment assets.

### On-demand behavior

- Normal chat loads application assets plus only the configured enabled extensions, unless safe mode bypasses them.
- Settings retrieves extension metadata, not every extension's CSS/JS or the stock source archive.
- Starting the extension builder supplies compact context, not the whole frontend source.
- Source reads/extraction happen only when the agent asks. Prefer the embedded archive directly; that path needs no checkout, npm install, internet access, or self-HTTP request. Published-source fallback may download an archive on demand.
- A remote agent may retrieve source from an authenticated, explicitly identified UI host using existing routing facilities. No arbitrary URL input or browser tokens in model context. Cross-node retrieval may follow the local demo; unsupported hosts must be reported honestly.
- Keep decompression lazy and source output bounded. Reuse extracted references only if archive digest and contents match. Full extraction returns paths/summary, not every source file in model context.
- Keep the archive out of ordinary browser payloads and service-worker caches. Builds without readable source use the published-source/compiled-assets fallback ladder, with explicit provenance.

## Specialist and optional authoring helpers

Add `extension-builder` using the existing builtin-agent/session patterns. Its instructions explicitly permit creating/editing extensions and updating enabled config to fulfill the request, subject to existing tool/workspace authorization. Inspect current source and config before changes; preserve unrelated files/settings. Do not copy all widget-builder shell privileges without need.

Ship manifest/config/reload/source-tool documentation in the binary. The useful workflow is inspect → edit files → enable in config → reload registry → refresh UI → iterate. Rollback is ordinary file/config repair, not a required built-in revision service. Safe mode makes the extension builder accessible for repair.

Provide only the authoring conveniences real examples need:

```js
export function activate(ui) {
  // Root/mount point, read-only UI context, session-change subscription,
  // and composer insertion are possible initial helpers.
}
```

Extensions can still access the DOM directly. Avoid exposing the entire private AppStore as a stable API. Recommend CSS and dedicated mounts over moving Preact-owned nodes. The terminal reads `--font-mono` when constructing xterm; arbitrary CSS changes do not guarantee live terminal restyling.

## Implementation milestones

### 1. Directory, config, and loader

- Add extension manifest discovery, directory/list config fields, schema entries, CLI overrides, and status/reload routes using widget conventions.
- Implement ordered CSS/JS loading, diagnostics, safe-mode bootstrap, and service-worker exclusions.
- Demonstrate two hand-authored extensions loaded together from config/flags without rebuilding.

Acceptance: config auto-loads the enabled set; CLI precedence and kill switch work; invalid entries are diagnosed; safe mode recovers a broken layout.

### 2. Minimal management UI and live config reload

- Add checkbox/order UI, Save and reload, Disable all, effective-source indicators, and Make UI Mine.
- Persist only extension config fields safely; preserve unrelated settings and detect concurrent updates.
- Reread config/rescan on explicit reload; show current-document versus new process state and flag overrides accurately.

Acceptance: changing the set persists across process restart, new page loads see it, flags remain authoritative, and no browser-local approval or preview state is required.

### 3. Source-aware agent — complete wow demo

- Embed readable build-matched source, publish versioned source archives, and add `ui_get_source` inventory/read/extraction with the best-effort fallback ladder.
- Add the specialist and refreshed extension/config/browser context.
- Let it inspect source, write CSS/JS, add its ID to config, reload, and continue after browser refresh.
- Demo reading typography plus compact navigation and a header clock, then remove the clock on request.

Acceptance: the agent can install and enable a working extension in a release deployment without a repository checkout, frontend rebuild, or mandatory publish/approve step.

### 4. Optional polish

File watching, CSS hot reload, cooperative JS disposal, extra authoring helpers, screenshot-assisted iteration, export/import, and a dedicated extension builder drawer can follow. None is required for the directory/config model.

## Verification

- Go: manifest/path/symlink validation; directory defaults; config/schema/flag precedence including explicit empty lists and disable flag; deterministic load order; missing/invalid IDs; authenticated status/assets/reload/config updates; preservation of unrelated config; concurrent update and read-only errors.
- Frontend: no extension requests in safe mode; ordered loading; per-extension errors; process config rather than localStorage selection; flag-controlled UI; reload and loaded-set reporting; safe-mode navigation/storage failure.
- Browser: CSS+JS extensions together; config auto-load after refresh/restart; disabling and JS reset via a new document; broken-UI safe-mode recovery; multiple tabs/process-wide scope; direct paths and Hub proxy assets; module auth/MIME; service-worker upgrades; edited-file cache invalidation and stale-generation errors.
- Source/build: matching archive/asset metadata; bounded reads and safe extraction; no injected secrets/public runtime archive; lazy loading; exact → published → compiled fallback; latest-published selection for dev builds and closest-lower semantic-version selection for release builds; offline/download-failure and no-older-release cases; provenance/mismatch labeling; stale browser/source-host mismatch; changed installed files versus browser-loaded fingerprints. Use fixtures, not live release downloads, in tests.
- Release/cache: curated archive attached with a checksum entry; missing historical assets; semantic-version selection across paginated fixtures; lookup-budget exhaustion; discovery TTL/ETag and rate-limit fallback; XDG/default cache roots; cache hits; concurrent/partial downloads; checksum failures; extraction limits; corrupt or modified cached trees. No live GitHub dependency in tests.
- Run relevant narrow tests, `make build`, root Go checks, frontend formatting/lint/typecheck/tests, and relevant Playwright coverage during implementation according to repository guidance. This document does not implement the feature.

## Non-goals

Mandatory browser-local activation, immutable publication/revision management, a general plugin marketplace, enforced JS sandboxing, complete stable frontend internals, a replacement chat engine, widget runtime changes, editing generated frontend assets, or making arbitrary code unbreakable.
