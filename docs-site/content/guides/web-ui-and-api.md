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
- `GET /ui/healthz`
- `GET /ui/` for the browser UI
- `GET /ui/images/:file` for generated images

If the jobs platform is also enabled, the jobs API is mounted under the same base path.

LLM job runs now expose a `session_id` and persist to the same sessions store by default, which makes web/API integrations much easier to inspect while a progressive run is still executing.

## Attachments

The browser UI accepts attachments from the paperclip button, drag/drop, and paste. The picker hints at the formats term-llm handles best: images (`png`, `jpeg`, `gif`, `webp`), PDFs, common text/data files (`txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`), and common Office document formats.

Server-side limits are authoritative: at most 10 attachments, 20 MB decoded per attachment, and 50 MB for the whole JSON request body. Base64 adds overhead, so multiple near-20 MB files may hit the request-body limit first.

File handling is provider-aware:

- Images are sent as image parts when the selected provider supports images.
- Providers with native file input support (currently OpenAI/ChatGPT/Copilot Responses transports by default) receive whitelisted MIME types as native file parts.
- Text-like uploads such as `txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`, and common code files are embedded as ordinary text when native file input is unavailable. Embedded contents are wrapped in explicit `BEGIN USER-PROVIDED FILE` / `END USER-PROVIDED FILE` markers.
- Unsupported binary files are saved locally and represented by a marker instead of being forwarded to the provider.

Do not attach secrets unless you intend the selected provider to receive them. Native file forwarding and text fallback both send file contents upstream.

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

## Useful flags

```bash
term-llm serve web \
  --provider anthropic \
  --agent assistant \
  --search \
  --mcp playwright \
  --max-turns 200 \
  --yolo
```

Relevant options include:

- `--provider`
- `--agent`
- `--search`
- `--native-search` / `--no-native-search`
- `--mcp`
- `--tools`, `--read-dir`, `--write-dir`, `--shell-allow`
- `--base-path`
- `--response-timeout` (defaults to `30m`; also configurable as `serve.response_timeout` with Go durations like `45m` or `1h`)
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
