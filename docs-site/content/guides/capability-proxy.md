---
title: "Capability proxy"
weight: 17
description: "Export configured term-llm providers as a capability-gated OpenAI/Anthropic HTTP service without sharing upstream credentials."
kicker: "Share providers safely"
---

`term-llm serve proxy` turns configured term-llm providers into a standalone HTTP service. Clients authenticate to the proxy with their own revocable, optionally expiring token; the proxy keeps the actual provider credentials and decides which provider/model aliases each client may call.

This is particularly useful for providers that are not ordinarily network services. A proxy host logged in to Claude Code can export `claude-bin/opus` or `claude-bin/haiku` through the OpenAI Responses API, allowing another container or server to consume that provider without receiving the Claude credential or needing the `claude` binary.

The proxy platform is currently a **prototype**. It supports model-provider export. Generic credential-injecting HTTP/TCP services, WebSocket transport, per-token quotas, and rate limiting are not included yet.

## Architecture and trust model

A typical deployment uses a separate container or server:

```text
┌─────────────────────────────┐
│ Client container            │
│                             │
│ client proxy token only     │
│ no provider credentials     │
└──────────────┬──────────────┘
               │ HTTPS
               ▼
┌─────────────────────────────┐
│ term-llm proxy              │
│                             │
│ provider credentials/login  │
│ clients, grants, audit      │
│ provider execution          │
└──────────────┬──────────────┘
               │
               ▼
        model providers
```

There are two separate authentication domains:

- The **admin token** unlocks the management UI and admin APIs. It can create clients, issue/revoke client tokens, and modify grants.
- A **client token** identifies one proxy consumer. It is accepted only by the client inference and self-service APIs.

The admin token is never accepted as a client token, and client tokens cannot call admin routes.

Client tokens are generated from cryptographically random bytes and stored in SQLite as SHA-256 hashes. The plaintext is displayed once when issued. Losing a client token requires issuing a replacement; the proxy cannot reveal it again.

Provider inference runs in a deliberately bare term-llm runtime:

- no agent or agent system prompt
- no memory or persisted conversation store
- no skills
- no MCP servers
- no server-executable tools, including search tools

Function/tool definitions supplied by an API caller can still be passed to the model and returned as tool calls for the **caller** to execute. They do not enable tools on the proxy host.

## Configure providers on the proxy host

The proxy exports provider/model aliases from the proxy host's normal term-llm configuration. For example:

```yaml
providers:
  claude-bin:
    type: claude-bin
    model: sonnet

  openai:
    type: openai
    api_key: op://Private/OpenAI/api-key
    model: gpt-5.5
```

`claude-bin` uses the Claude Code login available to the Unix user running the proxy. Log in on the proxy host before starting the service. Other providers use their normal term-llm credential configuration; see [Providers and models](/reference/providers-and-models/).

The proxy does not copy or send those credentials to clients.

## Start the proxy

```bash
export TERM_LLM_PROXY_ADMIN_TOKEN="$(openssl rand -hex 32)"

term-llm serve proxy \
  --host 127.0.0.1 \
  --port 8081 \
  --base-path /proxy
```

Startup prints the management URL, database path, and number of exported provider/model aliases:

```text
term-llm serve proxy listening on http://127.0.0.1:8081/proxy/
auth: bearer required
admin token: ... (from $TERM_LLM_PROXY_ADMIN_TOKEN)
proxy db: <data-dir>/proxy.db
exported provider/model aliases: 279
```

Important flags:

| Flag | Purpose |
|---|---|
| `--proxy-admin-token` | Stable admin token; falls back to `TERM_LLM_PROXY_ADMIN_TOKEN`, otherwise one is generated at startup |
| `--proxy-db` | SQLite database path; defaults to the term-llm data directory |
| `--base-path` | Mount path for both the UI and APIs; defaults to `/ui` |
| `--host`, `--port` | Listen address |
| `--response-timeout` | Maximum provider response-run duration |
| `--session-max`, `--session-ttl` | Bounds for in-memory, per-client conversation sessions |

The admin token is mandatory even when the server is loopback-only or started with `--no-auth`. Client tokens also remain mandatory because grants belong to a specific client.

For a remote deployment, terminate TLS in front of the proxy or place it on a private authenticated network. Do not expose plaintext HTTP carrying admin or client tokens across an untrusted network.

## Manage access from the mobile UI

Open the mounted proxy URL, such as:

```text
https://proxy.example.com/proxy/
```

Enter the admin token printed or configured at startup. The UI keeps it in JavaScript memory only; it is cleared when the UI is locked, the tab closes, or the page is reloaded.

The UI has five sections:

- **Overview** — active clients, pending requests, grants, and recent authorization activity
- **Clients** — create/disable clients and issue/revoke their tokens
- **Models** — grant or revoke provider/model access
- **Requests** — approve or deny access requested by clients
- **Activity** — authorization audit records

### Create a client and issue a token

1. Open **Clients** and choose **New client**.
2. Name the service, for example `jarvis` or `ci-reviewer`.
3. Choose **Issue token**.
4. Select an expiry such as one hour, one day, seven days, or a custom duration.
5. Copy the token from the one-time display and save it directly in the client service's secret storage.
6. Dismiss the display. The plaintext is removed from the UI and cannot be retrieved again.

Prefer expiring tokens. Use a non-expiring token only when the deployment has another reliable rotation mechanism.

Disabling a client rejects all of its tokens. Individual tokens can also be revoked without deleting the client or its grants.

### Grant a model

Open **Models**, select the client, and grant a concrete provider/model pair:

```text
claude-bin / haiku
```

A provider wildcard grants every model exported by that provider:

```text
claude-bin / *
```

Concrete grants are safer and easier to audit. Use wildcards for controlled internal clients that genuinely need model selection.

## Let clients request missing access

Clients do not need every future model preconfigured. If an authenticated client calls a model it cannot use, the proxy:

1. does **not** call the provider;
2. returns a structured `403`;
3. creates or reuses a pending access request for that client/provider/model;
4. displays the request in the admin UI.

Example denial:

```json
{
  "error": {
    "type": "access_denied",
    "code": "model_access_not_granted",
    "message": "access to \"claude-bin/opus\" is not granted; a pending access request has been recorded",
    "provider": "claude-bin",
    "model": "opus",
    "status": "pending",
    "request_id": "req-..."
  }
}
```

Open **Requests**, review the requesting client and provider/model, then approve or deny it. Approval creates the grant. The client can retry the original standard API request; no provider-specific reconfiguration is needed.

Pending requests are deduplicated and capped at **100 per client** to prevent an authenticated client from growing the database indefinitely with arbitrary model names.

Native clients can also explicitly request access:

```http
POST /proxy/v1/proxy/access-requests
Authorization: Bearer <client-token>
Content-Type: application/json

{
  "provider": "claude-bin",
  "model": "opus",
  "reason": "Review large code changes"
}
```

## Call the proxy

The proxy exposes three compatible inference surfaces under the configured base path:

```text
POST /proxy/v1/responses
POST /proxy/v1/chat/completions
POST /proxy/v1/messages
GET  /proxy/v1/models
```

`GET /v1/models` returns only models granted to the authenticated client.

### OpenAI Python client

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="https://proxy.example.com/proxy/v1",
    api_key=os.environ["TERM_LLM_PROXY_TOKEN"],
)

response = client.responses.create(
    model="claude-bin/haiku",
    input="Reply with exactly PROXY_OK.",
)

print(response.output_text)
```

The `api_key` here is the expiring **proxy client token**, not an OpenAI or Anthropic key.

### curl

```bash
curl https://proxy.example.com/proxy/v1/responses \
  -H "Authorization: Bearer $TERM_LLM_PROXY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-bin/haiku",
    "input": "Reply with exactly PROXY_OK."
  }'
```

### Anthropic Messages API

```bash
curl https://proxy.example.com/proxy/v1/messages \
  -H "x-api-key: $TERM_LLM_PROXY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-bin/sonnet",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Summarize this change."}]
  }'
```

Client authentication accepts either `Authorization: Bearer ...` or `x-api-key` for SDK compatibility.

Request bodies are capped at **8 MiB**.

## Automate client setup through the admin API

The mobile UI is the primary management surface, but deployments may bootstrap clients through the admin API.

Create a client:

```bash
client_id=$(
  curl -fsS https://proxy.example.com/proxy/admin/proxy/clients \
    -H "Authorization: Bearer $TERM_LLM_PROXY_ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name":"jarvis","description":"Personal assistant"}' \
  | jq -r '.client.id'
)
```

Issue a token that expires in one hour:

```bash
curl -fsS \
  "https://proxy.example.com/proxy/admin/proxy/clients/$client_id/tokens" \
  -H "Authorization: Bearer $TERM_LLM_PROXY_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"ttl_seconds":3600,"note":"initial deployment"}'
```

The `token` field in that response is returned only once.

Grant one model:

```bash
curl -fsS \
  "https://proxy.example.com/proxy/admin/proxy/clients/$client_id/grants" \
  -H "Authorization: Bearer $TERM_LLM_PROXY_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"provider":"claude-bin","model":"haiku","note":"approved model"}'
```

## Session isolation

Clients may use the normal `session_id` header and Responses API `previous_response_id` chaining. The proxy internally namespaces both by authenticated client identity.

Two clients choosing the same visible session ID do not share:

- provider runtime instances
- conversation history
- response IDs
- continuation authority

A client attempting to continue another client's response is rejected.

Session state is in memory only and is bounded by `--session-max` and `--session-ttl`. Restarting the proxy clears conversation sessions but does not remove clients, tokens, grants, access requests, or audit data from the proxy SQLite database.

## Security guarantees and limits

The prototype provides:

- provider credentials remain on the proxy host;
- client credentials are scoped to proxy grants and are useless against upstream providers directly;
- client tokens can expire or be revoked;
- admin and client authentication are separate;
- provider/model routing is pinned after authorization, so changing the request body cannot bypass a grant;
- client sessions and response chains are isolated;
- the provider runtime cannot execute term-llm server tools, skills, or MCP operations;
- authorization events are auditable.

It does **not** yet provide:

- per-client request, concurrency, cost, or token quotas;
- rate limiting;
- generic HTTP credential injection;
- arbitrary TCP proxying;
- WebSocket or OpenAI Realtime transport;
- multiple admin users, passkeys, or admin RBAC;
- encrypted proxy-database contents beyond filesystem protection.

Protect the proxy database and configuration with normal host permissions. Run the proxy under a dedicated Unix user in a separate container/server when isolation from agent shell access matters. Place provider credential files, OAuth sessions, and secret-manager access in that proxy security boundary—not in client containers.
