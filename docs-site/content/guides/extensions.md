---
title: "Make UI Mine: web extensions"
description: "Personalize term-llm's web interface with local CSS and JavaScript, or ask the extension builder to build a look for you."
---

Web extensions are trusted local CSS and JavaScript loaded by `term-llm serve web`. A theme can be CSS-only; a utility can use JavaScript. Multiple extensions can run together, in the order you choose. No frontend rebuild is needed.

## Make UI Mine

Open **Settings → Extensions → Make UI Mine**. This opens a conversation with the built-in `extension-builder`, or resumes an existing extension builder session.

Try:

> Make this feel like a quiet reading room. Warm colors, compact navigation, and slightly softer borders. Keep code easy to read.

The extension builder can inspect the app's actual source, write extension files, update the enabled list in config, and reload extension discovery. Its `ui_activate_extensions` tool requests automatic browser activation after its changes; no manual activation step is needed. Connected HTTP browsers reload when responses finish, drafts/attachments are clear, and dialogs are closed. Safe-mode tabs never auto-activate. The verified built-in web agent automatically receives file-tool access only to the extension directory, not the main config, its parent, or shell commands. Custom agents and child agents do not inherit this capability.

Extensions affect the web process, not only the browser that enabled them. Other clients see changes when they reload. The settings panel shows whether the enabled list comes from config or boot flags.

## Directory and manifest

By default, extensions live in `$XDG_CONFIG_HOME/term-llm/extensions`, or `~/.config/term-llm/extensions` when XDG_CONFIG_HOME is unset:

```text
extensions/
  extensions.yaml       # ordered enabled list
  dracula/
    extension.yaml
    style.css
  studio-clock/
    extension.yaml
    style.css
    main.js
```

Each immediate subdirectory is an extension. Its directory name is its ID: lowercase letters, numbers and hyphens, up to 64 characters, starting with a letter or number.

```yaml
title: My reading room
description: Warm paper, quiet borders, and a little more breathing room
format_version: 1
css: style.css
js: main.js
```

At least one of `css` or `js` is required. Entries, module imports, fonts and images use relative paths. Use browser-ready JavaScript modules, not TypeScript or JSX. Symlinks and paths outside the directory are not supported. Each extension may contain up to 512 supported assets; enabled assets share a 32 MiB snapshot budget.

Example theme and clock directories are available in the repository under `examples/extensions/`.

## Enable extensions

Add an ordered list to `extensions/extensions.yaml`:

```yaml
enabled:
  - dracula
  - studio-clock
```

Or use boot flags:

```sh
term-llm serve web --extensions dracula,studio-clock
term-llm serve web --extensions-dir ./extensions --extensions dracula
term-llm serve web --extensions ''
term-llm serve web --disable-extensions
```

`serve.extensions_dir` overrides the default directory. For the enabled list, explicit flags override `extensions.yaml`, which overrides legacy `serve.extensions` in the main config (used only when the local file is absent). An explicitly empty list disables all extensions. `--disable-extensions` is a process-wide kill switch that cannot be overridden through the web UI.

In Settings, check the extensions you want, reorder them with the arrows, and select **Save and reload UI**. **Disable all** clears the selection; save to persist it. Saves update only `extensions.yaml`, preserving comments; the main config is never rewritten. If another client changed config, refresh the list before saving again.

After editing files/config manually, use **Rescan files**, then refresh the browser. The extension builder's `ui_extensions` tool can also rescan without reloading browsers. Its separate `ui_activate_extensions` tool rescans and requests automatic activation; it reports a request, not confirmation of rendering. Boot overrides remain authoritative after a rescan.

## Loading and updates

The server validates and snapshots enabled assets on startup or explicit rescan. A running page never reads half-written files directly. CSS is loaded in enabled-list order, followed by each extension's JavaScript activation. Later CSS obeys the normal cascade.

The browser uses generation-scoped URLs and extension assets are excluded from the service-worker shell cache. After a rescan, requests for an old generation receive a reload-needed response. A full page reload resets JavaScript effects; there is no hot-uninstall or mandatory publication/revision system.

Direct HTTP and Hub-proxied HTTP connections are supported. Browser module loading over a WebRTC-only connection is not currently supported; use a direct or Hub HTTP connection for extensions.

## JavaScript helpers

An extension may export `activate(ui)`:

```js
export function activate(ui) {
  const label = document.createElement('span');
  label.textContent = 'My workspace';
  ui.mount.append(label);

  ui.onSessionChanged((sessionId) => {
    label.title = sessionId || 'New conversation';
  });

  ui.onContextUsageChanged((usage) => {
    if (!usage) {
      label.textContent = '';
      return;
    }
    const limit = usage.inputLimit ? `/${usage.inputLimit}` : '';
    label.textContent = `~${usage.usedTokens}${limit}`;
  });
}
```

The host provides:

- `root`: the Preact application root.
- `mount`: a dedicated element outside Preact ownership for your extension.
- `getContext()`: the current session ID, frontend asset version and app prefix.
- `onSessionChanged(callback)`: subscribes immediately and on changes; returns an unsubscribe function.
- `getContextUsage()`: the active session's latest approximate context snapshot, or `null`. The snapshot contains `usedTokens`, optional `inputLimit`, cumulative `cachedInputTokens`, and `estimated`.
- `onContextUsageChanged(callback)`: delivers that snapshot immediately and after session loads, changes, or completed responses; returns an unsubscribe function.
- `insertComposerText(text)`: appends draft text without submitting it.

Context usage deliberately differs from aggregate response usage: `usedTokens` approximates current context occupancy, while `cachedInputTokens` is cumulative session accounting. Keep an approximation marker such as `~`, and omit percentages when `inputLimit` is unavailable.

Direct DOM access is allowed, but prefer CSS and dedicated mounts to moving Preact-owned nodes. Private selectors may change between releases. Canvas-rendered terminal content does not automatically restyle when CSS changes.

## Recovery

Open the current web URL with **`?safe-mode=1`**, preferably in a fresh tab if JavaScript has frozen the original page. Safe mode skips all extension CSS and JS. It stays on for that tab until you explicitly exit it, including across session navigation.

The stock settings panel remains available for repair or disabling extensions. Saving configuration does not exit safe mode automatically. To disable extensions for the whole process, restart with `--disable-extensions` or remove the enabled list from config.

**Extensions are trusted application code, not sandboxed widgets.** They can read displayed conversations and act with the browser's authenticated access. Safe mode recovers UI breakage; it does not undo network requests, data disclosure or malicious persistent changes. Only enable code you trust.

## Source inspection and cache

The specialist has two opt-in tools:

- `ui_extensions`: current directory/config/flag provenance and reload, plus browser-loaded context when available.
- `ui_get_source`: list, bounded read, or approved extraction of stock readable source; `target: compiled` exposes the exact shipped HTML/CSS/JS. Installed extension files are inspected with the ordinary file tools using paths from status.

New release builds privately embed a compressed readable-source archive. It is extracted/read only on demand, not sent to ordinary browser sessions or dumped wholesale into model context. Source inspection never includes rendered HTML with injected credentials.

When exact readable source is unavailable, the resolver tries published source: most recent stable release for development builds, or the closest suitable release at/below the running version. Failing that, it returns compiled/minified assets. Provenance and mismatches are explicit; older source is guidance, not an exact representation of the running UI.

New releases publish:

```text
https://github.com/samsaffron/term-llm/releases/download/vX.Y.Z/term-llm-ui-source.tar.gz
https://github.com/samsaffron/term-llm/releases/download/vX.Y.Z/checksums.txt
```

Downloads are checksum-verified and cached under `$XDG_CACHE_HOME/term-llm/ui-source/` (default `~/.cache/term-llm/ui-source/`). Release metadata has a 24-hour freshness window; verified archives are content-addressed beneath `releases/<tag>/<sha256>/`. References are decompressed lazily in memory; explicit tool extraction writes a new approved workspace directory. Deleting the cache is safe. Offline/rate-limited requests use cached or compiled references. Older releases without the new artifact are skipped.

## Clean-room smoke test

From a source checkout:

```sh
bash scripts/extensions_smoke.sh
```

This builds the app, creates an isolated home/config/workspace with no user credentials, installs Dracula plus a JavaScript clock, exercises enabling and settings, deliberately breaks the theme, and repairs it through safe mode. It runs desktop/mobile Chromium tests and retains screenshots under `.cache/extensions-smoke/`. It uses the debug provider for infrastructure testing, not a live-model design evaluation.
