---
title: "Multi-agent gateway"
weight: 16
description: "Front several per-agent serves behind one origin, inject each agent's token server-side, and serve widgets from an isolated origin."
kicker: "Run many agents"
---

`term-llm serve-gateway` is a thin reverse proxy that fronts the per-agent web serves of several [agent containers](/guides/agent-containers/). A `term-llm serve` binds a single agent at startup, so multi-agent access is achieved by *fronting many serves* rather than routing many agents inside one process — which is already the production shape: one container per agent.

The gateway does two jobs:

1. **Agent proxy** — exposes every discoverable agent's web UI under one origin and injects that agent's bearer token server-side, so the per-agent token never reaches the browser.
2. **Widget host** — optionally serves each agent's widgets from a *separate* origin, so an embedded widget is isolated to a throwaway origin that hosts nothing sensitive.

> **Experimental — loopback only.** The gateway has no authentication of its own yet. It refuses to bind to anything but a loopback address. To expose it, put an authenticating reverse proxy in front and keep the gateway on loopback.

## Quick start

```bash
# Create and start a couple of agents (see the Agent Containers guide).
term-llm contain new fam   --template agent
term-llm contain new ops   --template agent
term-llm contain start fam
term-llm contain start ops

# Run the gateway. It discovers agents from their contain workspaces.
term-llm serve-gateway --port 8090
```

Open `http://localhost:8090/` for the landing page: a list of discoverable agents, each linking to its full web UI through the proxy. Clicking an agent opens its UI at `/agent/<name>/` with the token injected for you.

## How agent discovery works

The gateway enumerates contain workspaces and reads each one's `.env` for its published port and bearer token (the same values `term-llm contain port <name>` and `term-llm contain token <name>` print). An agent shows as **reachable** once it has a provisioned `WEB_TOKEN`; agents without one are listed but not proxyable.

Routes on the agent origin:

| Route | Purpose |
| --- | --- |
| `GET /agents` | JSON list of discoverable agents (**never** includes tokens) |
| `GET /` | HTML landing page |
| `ANY /agent/<name>/...` | reverse proxy to that agent's serve |

The proxy injects the per-agent `Authorization: Bearer` token, strips any client-supplied `Authorization`, `Cookie`, and `X-Api-Key`, and drops spoofable `X-Forwarded-*` headers. It also rebases the agent's baked-in `/chat` URL prefix onto `/agent/<name>` so the single-page UI's API calls, service worker, and subresources all route back through the proxy — no changes to the agent needed.

## The widget host (separate origin)

Widgets are live, per-agent web apps. Serving them from the *same* origin as something sensitive is dangerous: a prompt-injected widget granted `allow-same-origin` runs as that origin. The fix is to serve widgets from a dedicated origin that hosts **nothing** except widgets.

Enable it by giving the gateway a widget hostname:

```bash
term-llm serve-gateway --port 8090 --widget-host widgets.localhost
```

The same listener now answers on two origins:

- the **agent origin** (e.g. `localhost:8090`) — agents, landing page, `/agents`;
- the **widget origin** (e.g. `widgets.localhost:8090`) — **only** `/w/<agent>/<mount>/...`. Every other path 404s there.

A widget request to `http://widgets.localhost:8090/w/fam/job-usage/` is proxied to that agent's serve at `{WEB_BASE_PATH}/widgets/job-usage/`, with the token injected exactly as for the agent proxy. Because the browser treats `widgets.localhost:8090` as a distinct origin from `localhost:8090`, a widget iframe is confined to that empty origin.

Widget responses are passed through unchanged — widgets resolve their assets against their own origin via relative URLs, so no prefix rewriting is applied.

When `--widget-host` is set, the landing page also lists each reachable agent's widgets (queried from that agent's serve), grouped under their agent, as links that open directly on the widget origin — so you do not have to construct the URLs by hand.

### Making the widget origin resolve

The same-origin policy is per *origin*, so the widget host must be a real hostname, not just a path. For a browser you need the hostname to resolve to the gateway:

```bash
# Simplest: a hosts-file entry.
echo '127.0.0.1 widgets.localhost' | sudo tee -a /etc/hosts
```

On startup the gateway resolves the widget host and prints a warning if it does not resolve (so you are not left wondering why widget links 404 in the browser), or if it resolves to a non-loopback address.

For CLI testing you can skip DNS entirely by setting the `Host` header:

```bash
curl -s -H 'Host: widgets.localhost' http://127.0.0.1:8090/w/fam/job-usage/
```

In production, point a `widgets.<your-host>` subdomain at the gateway.

## Security posture

- **No gateway auth yet.** Anyone who can reach the gateway can reach every discoverable agent. The gateway therefore refuses non-loopback binds — keep it on `127.0.0.1`/`localhost`/`::1` and front it with your own authenticating proxy to expose it.
- **Tokens stay server-side.** Per-agent tokens are read from each workspace's `0600` `.env` and injected by the proxy; `/agents`, the landing page, and the browser never see them.
- **Path safety.** Encoded path separators (`%2f`, `%5c`) and `..` segments are rejected; agent names and widget mounts are validated against their allowed character sets before any backend request.
- **Bounded backends.** The proxy uses bounded dial and response-header timeouts so a hung agent can't tie it up, while leaving streaming responses (SSE) open.

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--host` | `127.0.0.1` | Bind host (must be loopback) |
| `--port` | `8090` | Bind port |
| `--agent-host` | `127.0.0.1` | Host the per-agent serves are published on |
| `--widget-host` | `widgets.localhost` | Dedicated origin for the widgets-only proxy (empty to disable) |

## Limitations and roadmap

- **No access grant yet.** The widget origin currently proxies any `/w/<agent>/<mount>/` request (loopback only). A signed, expiring access grant — so a widget link can be handed out and validated cross-origin — is the next step.
- **Shared widget origin.** All widgets share one origin today, so they can read each other's `localStorage`. Acceptable within one trusted family; a per-agent widget subdomain is later hardening.
