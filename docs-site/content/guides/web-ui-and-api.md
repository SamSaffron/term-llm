---
title: "Web UI and API"
weight: 7
description: "Run term-llm as a web server, use the browser UI, and call the HTTP API endpoints exposed by serve mode."
kicker: "Web runtime"
featured: true
next:
  label: WebRTC direct routing
  url: /guides/webrtc-direct-routing/
---
## Start the web runtime

```bash
term-llm serve web
```

Useful variants:

```bash
term-llm serve api                 # API only (no chat UI)
term-llm serve web --base-path /chat
term-llm serve web --title "My Lab"
term-llm serve web --host 127.0.0.1 --port 8080
term-llm serve web jobs
term-llm serve web jobs telegram   # all platforms at once
```

## First-time setup

Use `--setup` to run the interactive credential wizard for the selected platforms:

```bash
term-llm serve web --setup
```

Re-run with `--setup` any time to update stored credentials.

## Default platforms

To avoid specifying platforms every time, set them in `config.yaml`:

```yaml
serve:
  platforms:
    - web
    - jobs
```

`term-llm serve` with no positional arguments reads from `serve.platforms`.

## What it serves

With the default base path of `/ui`, the web runtime exposes:

- `POST /ui/v1/responses`
- `POST /ui/v1/chat/completions`
- `POST /ui/v1/messages` (Anthropic Messages API)
- `POST /ui/v1/transcribe`
- `GET /ui/v1/models`
- `GET /ui/v1/capabilities`
- `GET|POST /ui/v1/projects` (`POST ...?dry_run=1` previews normalization)
- `GET|PATCH /ui/v1/projects/:id`
- `GET|POST /ui/v1/projects/:id/worktrees`
- `DELETE /ui/v1/projects/:id/worktrees?dir=...`
- `GET /ui/v1/projects/:id/worktrees/diff?dir=...`
- `POST /ui/v1/projects/:id/worktrees/merge`
- `POST /ui/v1/projects/:id/worktrees/promote`
- `GET /ui/v1/sidebar`
- `GET /ui/v1/sessions?project_id=prj_...&cursor=...`
- `GET /ui/v1/sessions/search?q=...&project_id=prj_...`
- `POST /ui/v1/sessions/:id/project` (validated one-time historical assignment)
- `GET /ui/healthz`
- `GET /ui/` for the browser UI
- `GET /ui/images/:file` for generated images

If the jobs platform is also enabled, the jobs API is mounted under the same base path.

LLM job runs now expose a `session_id` and persist to the same sessions store by default, which makes web/API integrations much easier to inspect while a progressive run is still executing.

## Project-aware Responses and worktrees

The built-in Web UI fetches `GET /ui/v1/capabilities` before rendering project controls and uses the bounded grouped `GET /ui/v1/sidebar` projection. Project creation supports `POST /ui/v1/projects?dry_run=1` to preview canonical server-path and Git-root normalization before writing. Project records can be renamed, archived, and restored, but not hard-deleted or moved in place.

A fresh project-aware Responses request includes:

```json
{
  "project_id": "prj_...",
  "worktree_dir": "/optional/term-llm-managed-worktree"
}
```

The server resolves the stable ID, revalidates the canonical path, verifies managed-worktree repository ownership, and atomically snapshots the binding before execution. Repeating the same binding is idempotent; conflicting project/worktree values return `409 workspace_conflict`. First-party UI requests require `project_id` in project mode. Authenticated third-party Responses clients may supply it, but omission keeps their existing unbound/explicit behavior.

The grouped sidebar request is `GET /ui/v1/sidebar?per_project=12&include_archived_projects=1&include_archived_sessions=0`. It returns active, archived, empty, and optional **No project** groups in one bounded projection. Each group carries `session_count`, `last_activity_at`, up to `per_project` summaries, and an opaque `next_cursor`. Pass that cursor back only for the same group; the null-project cursor is sent without `project_id`. Global full-text search results include `project_id` and `project_name` so clients can regroup them without racing a second project lookup.

Project-scoped worktree routes are under `/ui/v1/projects/:id/worktrees`: collection `GET`/`POST`, `DELETE ...?dir=...`, `GET .../diff?dir=...`, and `POST .../merge` or `.../promote`. The old `/ui/v1/worktrees` routes remain for one compatibility release, always target the serve startup repository, and return a deprecation header. They never follow browser-selected project state.

Stable project/workspace errors use the normal authenticated JSON error envelope:

| Code | Meaning and recovery |
|---|---|
| `project_required` | A fresh first-party project-mode conversation omitted its project; choose or add one. |
| `project_not_found` | The registry identity no longer exists; refresh projects and choose another. |
| `project_archived` | An archived project cannot start a new conversation; restore it or use another. |
| `project_unavailable` | The path is missing or its canonical identity changed; repair it or archive/re-add the moved path. |
| `workspace_conflict` | The immutable session binding already won with different values; refetch that session. |
| `worktrees_unavailable` | The selected project is not an available Git repository. |
| `projects_disabled` | Project mode is disabled. Project routes return authenticated `404`; Responses rejects supplied `project_id` with `400`. |
| `refresh_required` | An obsolete first-party asset requested legacy default binding without a usable bootstrap; hard-refresh the page/service-worker assets. |

In `--no-projects` single-workspace mode, the project/sidebar mutation routes expose `projects_disabled`, the browser uses the flat/date navigator, and unsent project drafts are discarded. A first-party `use_default_workspace` request binds to the serve startup directory's main Git root, while a non-Git startup remains unbound. Header-less third-party requests remain unbound when they omit `project_id`.

All project routes use the existing bearer-token and CORS middleware. The token identifies one shared operator security domain, not separate users or tenants. Project paths are therefore visible to every authenticated token holder, and the first-party UI version header is only a compatibility signal—not authorization.

## Live diff sidebar

When [file change tracking](/reference/configuration/#file-change-tracking-config) is enabled, the browser UI shows a right-hand "Changes" panel for sessions in which agent tools modify files. Files appear as the agent edits them, expand inline to show the cumulative diff for the session (baseline = the file's state when the session first touched it), and can be collapsed individually. The panel is resizable and can be dismissed per session.

Tracking is opt-in because it persists file contents to a local database — see the privacy note in the configuration reference. Changes made by shell commands are tracked best-effort: precise when the command declares `affected_paths`, otherwise inferred from `git status` and previously tracked files.

## Attachments

The browser UI accepts attachments from the paperclip button, drag/drop, and paste. The picker hints at the formats term-llm handles best: images (`png`, `jpeg`, `gif`, `webp`), PDFs, common text/data files (`txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`), and common Office document formats.

Server-side limits are authoritative: at most 10 attachments, 20 MB decoded per attachment, and 50 MB for the whole JSON request body. Base64 adds overhead, so multiple near-20 MB files may hit the request-body limit first.

File handling is provider-aware:

- Images are sent as image parts when the selected provider supports images.
- Providers with native file input support (currently OpenAI, ChatGPT, Grok subscription, and Copilot Responses transports by default) receive supported files as native Responses inputs. The default native MIME set covers PDF; `text/*`; JSON/XML; Word/RTF/OpenDocument text; Excel; and PowerPoint files.
- Text-like uploads such as `txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`, and common code files are embedded as ordinary text when native file input is unavailable. Embedded contents are wrapped in explicit `BEGIN USER-PROVIDED FILE` / `END USER-PROVIDED FILE` markers.
- Unsupported binary files are saved locally and represented by a marker instead of being forwarded to the provider.

Do not attach secrets unless you intend the selected provider to receive them. Native file forwarding and text fallback both send file contents upstream.

## Current location

The browser UI's **+** menu can request your current location and add coordinates, reported accuracy, and an OpenStreetMap link to the composer. It never requests location on page load and does not send automatically: review or edit the text, then press Send. Geolocation requires HTTPS or localhost and remains subject to the browser's permission controls.

Coordinates become ordinary chat content when sent. They can be persisted in session history, included in later model context, and forwarded to the selected provider. term-llm does not call a reverse-geocoding service.

Administrators can hide the action either with `term-llm serve --disable-location-sharing` or in `config.yaml`:

```yaml
serve:
  disable_location_sharing: true
```

## GPT-5.6 Responses controls

The browser model picker reads `reasoning_efforts` and `reasoning_modes` from `GET /ui/v1/models`. For OpenAI API GPT-5.6 models it shows a **Standard / Pro** selector and sends the selection as `reasoning.mode`. The selector is hidden for unsupported models; switching to one clears a stale Pro selection. Codex's product-level Ultra option is not shown as an effort because its inference request uses `max` and separately enables subagents.

API clients can send GPT-5.6 advanced controls to `POST /ui/v1/responses`:

```json
{
  "provider": "openai",
  "model": "gpt-5.6-terra",
  "input": "Investigate the failures and propose a fix",
  "reasoning": {
    "effort": "high",
    "mode": "pro",
    "context": "all_turns"
  },
  "multi_agent": {
    "enabled": true,
    "max_concurrent_subagents": 3
  },
  "prompt_cache_options": {
    "mode": "explicit",
    "ttl": "30m"
  },
  "stream": true
}
```

Accepted values are:

- `reasoning.effort`: OpenAI GPT-5.6 supports `none`, `low`, `medium`, `high`, `xhigh`, and `max`.
- `reasoning.mode`: `standard` or `pro`.
- `reasoning.context`: `auto`, `current_turn`, or `all_turns`.
- `prompt_cache_options.mode`: `implicit` or `explicit`; `ttl` currently supports only `30m`.
- `multi_agent.max_concurrent_subagents`: defaults to `3` when multi-agent is enabled and the value is omitted. Multi-agent requests use HTTP/SSE even if WebSocket transport is configured, pending WebSocket `response.inject` support.

Programmatic tool calling is requested with an eligible function tool plus the PTC marker tool:

```json
{
  "tools": [
    {
      "type": "function",
      "name": "read_file",
      "description": "Read a file",
      "parameters": {"type": "object", "properties": {"path": {"type": "string"}}},
      "allowed_callers": ["programmatic"]
    },
    {"type": "programmatic_tool_calling"}
  ]
}
```

Every programmatic tool must be present in the request. Optional `output_schema` metadata is preserved for tool definitions.

These fields are deliberately gated to the built-in `openai` provider's GPT-5.6 family. Requests using older OpenAI models, custom OpenAI-compatible providers, or ChatGPT OAuth fail validation instead of forwarding unsupported controls. Do not send `reasoning.mode: pro` merely because a ChatGPT account has Pro subscription access. Likewise, Codex Ultra is a product-level multi-agent option, not a `reasoning.effort` wire value.

Opaque provider replay items used to continue stateless Responses turns are persisted internally but never rendered in the browser or included in session exports.

## Persistent goals in the browser UI

The browser UI exposes the same persistent goal state as terminal chat. Open the composer `+` menu and choose **Set goal…**, or click the `🎯` goal chip above the composer after a goal exists. Goals are stored on the session and survive page reloads; an active goal lets the shared runner automatically continue work until the model marks it complete/blocked, the user pauses or clears it, the run is stopped, or an optional token budget is exhausted.

For API integrations, goal state is available from the session state endpoint and can be mutated with:

```bash
curl -X POST "$BASE/ui/v1/sessions/$SESSION_ID/runtime/goal" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"action":"set","objective":"finish the migration and verify tests","token_budget":50000}'
```

Supported actions are `set`, `edit`, `pause`, `resume`, and `clear`. `GET /ui/v1/sessions/:id/state` includes `goal` (or `null`) so clients can render status and token usage.

## Authentication

By default, serve mode uses bearer-token auth.

```bash
term-llm serve web --token "$TOKEN"
```

If you omit `--token`, term-llm can generate one automatically.

### Persist the token across restarts

Without `--token`, a fresh bearer token is generated on every start, which means any saved client config (browser tabs, scripts, API clients) breaks after a restart. Set `TERM_LLM_SERVE_TOKEN` in your environment to keep the same token across restarts.

Note that `export FOO=...` only persists for the current shell session — close the terminal or reboot and the value is gone. To survive across sessions, add it to your shell's startup file:

```bash
# bash / zsh: append to your rc file
echo "export TERM_LLM_SERVE_TOKEN=\"$(openssl rand -hex 32)\"" >> ~/.bashrc
# (or ~/.zshrc)
```

```fish
# fish: -U makes it a universal variable (persists across sessions), -x exports it
set -Ux TERM_LLM_SERVE_TOKEN (openssl rand -hex 32)
```

Then start the server in a new shell:

```bash
term-llm serve web
```

Precedence: `--token` > `$TERM_LLM_SERVE_TOKEN` > auto-generated.

You can disable auth only on loopback hosts:

```bash
term-llm serve web --auth none --host 127.0.0.1
```

`--allow-no-auth` and `--auth none` are only valid for loopback use. Exposing an unauthenticated server beyond localhost would be idiotic.

## Run as a systemd user service

For a persistent Linux deployment, the repository includes a complete
[`serve web` systemd example](https://github.com/samsaffron/term-llm/tree/main/examples/systemd-serve-web).
It provides an interactive installer that finds the binary, generates a stable
bearer token, optionally records provider and reverse-Hub credentials, writes
and starts the user units, and prints the maintenance commands when it
finishes. The installed 04:00 timer restarts the service only when the installed
`term-llm` binary differs from the running executable.

The same example can optionally register the Web UI as a reverse-connected Hub
node. Its environment template documents the Hub URL and node arguments,
`TERM_LLM_HUB_REGISTRATION_TOKEN`, and how the stable
`TERM_LLM_SERVE_TOKEN` is used as the node bearer token.

## Useful flags

Ordinary serve platforms default to Guardian-reviewed auto approval. To require human approval for unmatched actions, launch with `--approval prompt` (or set `serve.approval_mode: prompt`). Auto is fail-closed: serve startup fails if Guardian cannot initialize.

```bash
term-llm serve web \
  --provider anthropic \
  --agent assistant \
  --search \
  --mcp playwright \
  --max-turns 200 \
  --approval prompt
```

Relevant options include:

- `--provider`
- `--agent`
- `--approval prompt|auto|yolo` (`--auto` and `--yolo` are aliases)
- `--search`
- `--native-search` / `--no-native-search`
- `--mcp`
- `--tools`, `--read-dir`, `--write-dir`, `--shell-allow`
- `--base-path`
- `--title` (overrides the web UI sidebar title; also configurable as `serve.title`)
- `--response-timeout` (maximum active execution time, default `30m`; the clock pauses while waiting for approval or `ask_user`; also configurable as `serve.response_timeout` with Go durations like `45m` or `1h`)
- `--cors-origin`
- `--webrtc`, `--webrtc-signaling-url`, `--webrtc-token` (see [WebRTC direct routing](/guides/webrtc-direct-routing/))

## Health checks

Typical checks:

```bash
curl http://127.0.0.1:8080/ui/healthz
curl http://127.0.0.1:8080/ui/v1/models
```

If you change `--base-path`, those URLs change with it.

## API-only mode

Use the `api` platform when you only need the HTTP API without the browser UI:

```bash
term-llm serve api -p anthropic
```

This is useful for headless deployments or when using term-llm as a backend
for tools like Claude Code that speak the Anthropic Messages API.

Authentication accepts both `Authorization: Bearer <token>` and `x-api-key: <token>` headers.

### Tool mapping

When the API client sends tool definitions with different names than the server's
registered tools, use `--tool-map` to redirect them. For example, Claude Code
sends `WebSearch` and `WebFetch`, but term-llm registers `web_search` and `read_url`:

```bash
term-llm serve api -p my_provider --search \
  --tool-map "WebSearch:web_search" \
  --tool-map "WebFetch:read_url"
```

The server intercepts calls to the client tool name and executes the mapped
server tool instead. The client tool definition is sent to the backend LLM
while the server tool is hidden. If a `--tool-map` target doesn't match a
registered server tool, startup fails with the list of available tools.

## When to use web mode

Use the web runtime when you want:

- a browser UI instead of terminal chat
- an HTTP API surface for integrations
- a shared local service with authentication
- combined web and jobs runtime on one port

## Related pages

- [WebRTC direct routing](/guides/webrtc-direct-routing/)
- [Jobs](/guides/job-runner/)
- [Telegram Bot](/guides/telegram-bot/)
- [Configuration](/reference/configuration/)
- [Search](/guides/search/)
