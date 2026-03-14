---
title: "Web UI and API"
weight: 7
description: "Run term-llm as a web server, use the browser UI, and call the HTTP API endpoints exposed by serve mode."
kicker: "Web runtime"
featured: true
next:
  label: Search
  url: /guides/search/
---
## Start the web runtime

```bash
term-llm serve web
```

Useful variants:

```bash
term-llm serve web --no-ui
term-llm serve web --base-path /chat
term-llm serve web --host 127.0.0.1 --port 8080
term-llm serve web jobs
```

## What it serves

With the default base path of `/ui`, the web runtime exposes:

- `POST /ui/v1/responses`
- `POST /ui/v1/chat/completions`
- `POST /ui/v1/transcribe`
- `GET /ui/v1/models`
- `GET /ui/healthz`
- `GET /ui/` for the browser UI
- `GET /ui/images/:file` for generated images

If the jobs platform is also enabled, the jobs API is mounted under the same base path.

LLM job runs now expose a `session_id` and persist to the same sessions store by default, which makes web/API integrations much easier to inspect while a progressive run is still executing.

## Authentication

By default, serve mode uses bearer-token auth.

```bash
term-llm serve web --token "$TOKEN"
```

If you omit `--token`, term-llm can generate one automatically.

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
- `--no-ui`
- `--cors-origin`

## Health checks

Typical checks:

```bash
curl http://127.0.0.1:8080/ui/healthz
curl http://127.0.0.1:8080/ui/v1/models
```

If you change `--base-path`, those URLs change with it.

## When to use web mode

Use the web runtime when you want:

- a browser UI instead of terminal chat
- an HTTP API surface for integrations
- a shared local service with authentication
- combined web and jobs runtime on one port

## Related pages

- [Jobs](/guides/job-runner/)
- [Configuration](/reference/configuration/)
- [Search](/guides/search/)
