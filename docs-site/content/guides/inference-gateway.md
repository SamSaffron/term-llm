---
title: "Inference Gateway"
weight: 18
description: "Centralize provider credentials and web access while keeping agents, tools, approvals, and files on satellites."
kicker: "Private provider plane"
---

The inference gateway centralizes LLM provider credentials, OAuth/CLI homes, model discovery, web search/fetch credentials, and attributed usage. It is a remote **provider**, not a remote agent.

A satellite still owns:

- the engine and agent/system prompt
- session history, compaction, jobs, memory, and UI
- the local tool registry and approval policy
- its working directory and filesystem

The gateway owns provider transport only. Satellites continue using ordinary `provider:model` selections; catalog providers are routed through the gateway automatically.

## Protocol and trust boundary

Gateway traffic has two authenticated provider edges:

- the private, versioned `/g1` HTTP/SSE protocol used by term-llm satellites
- an OpenAI-compatible `POST /v1/responses` and `GET /v1/models` edge for Discourse and other provider clients

Neither edge is a full-agent runtime. `/v1/responses` translates directly to the same provider-neutral request/event stream used by `/g1`; it does not proxy through `/g1`, start `term-llm serve`, load sessions/skills/jobs/memory, or create a gateway-side tool registry. The private protocol additionally supports ETagged catalog discovery, cancellation, authenticated synchronous satellite tool callbacks, sealed provider state, enrollment, search, fetch, and health.

Important invariants:

1. Provider API keys, OAuth tokens, CLI homes, and provider configuration are never returned by catalog or inference endpoints.
2. A satellite `WorkingDir` is not a wire field. Each gateway request gets a new empty directory under the gateway state-owned `runs/` root. Finished runs are removed, and stale gateway-prefixed directories are scavenged safely at startup.
3. The gateway has no satellite or external-client tool registry. Normal `/g1` tool calls return to the satellite engine. Inline CLI-provider calls on `/g1` use an authenticated callback POST and block the gateway provider until the satellite result arrives. `/v1/responses` accepts only client-defined function tools, returns function calls to the client, and never executes them; tool-bearing requests to incompatible inline-loop CLI providers are rejected before provider startup.
4. Provider resume state is opaque to satellites and authenticated with an AES-GCM gateway key. It is bound to protocol version, authenticated client ID, provider key, and the satellite `SessionID`; tampered, cross-client, cross-provider, or cross-session state is rejected. Requests with an empty `SessionID` are deliberately stateless and neither import nor export sealed provider state.
5. Each request is recorded centrally with client, provider key, model, request/session IDs, token counters, outcome, and locally calculable cost.
6. The gateway performs provider retries. By default it makes at most three upstream attempts within 20 seconds; `--upstream-retry-attempts` and `--upstream-retry-elapsed` tighten or extend those bounds. Request cancellation and stream idle deadlines still win. The satellite gateway transport and engine do not add a second retry loop.

### Session continuity and direct-provider equivalence

For an ordinary satellite session, the satellite remains the source of truth for `SessionID` and the complete transcript. It stores provider state under that session plus provider key, includes the same `SessionID` and provider-facing history on later turns, and can export/import the `GatewayProvider`'s opaque sealed blob when reconstructing a runtime. Providers that implement provider-state export/import continue to round-trip that state through the session-bound sealed blob; providers that do not can always reconstruct from the complete transcript.

For sessionful satellite `/g1` inference, the gateway keeps a successfully completed central provider warm for **30 seconds** only when the request has a non-empty `SessionID`, is not ephemeral, and that explicit central OpenAI/ChatGPT provider has `use_websocket: true`. A lease is isolated by authenticated gateway client ID, provider key, and satellite `SessionID`, and turns for one lease are serialized. Other clients, providers, and sessions remain independent and concurrent. The model is deliberately not part of the lease key: this matches a direct provider instance and permits model changes, while `ResponsesClient` rejects an incompatible `previous_response_id` continuation and starts a full-history chain on the same connection when model or other non-input controls change.

A warm follow-up uses the same provider and WebSocket, so ChatGPT sends the same `previous_response_id` plus continuation-only suffix as direct term-llm. The public `/v1/responses` edge does not opt into these private session semantics and remains stateless; unrelated Discourse requests are never attached to a retained satellite provider. WebSocket providers currently export no sealed provider state, so a warm provider is used directly rather than importing the satellite's prior blob a second time. Exportable-state providers and ordinary non-WebSocket providers retain the sealed per-request behavior above and are not added to this cache.

Idle expiry is refreshed only after a fully successful turn. Cancellation, provider/stream failure, invalid continuation state, an incompatible config/policy decision, credential/config generation change, or gateway shutdown evicts the lease and calls provider conversation reset/cleanup through the retry wrapper. Idle eviction and shutdown close the WebSocket. The cache uses one bounded reaper rather than one timer or goroutine per session.

After idle expiry or gateway restart, the next request creates a provider and sends the satellite's complete transcript without `previous_response_id`. This cold form is semantically correct and matches the direct provider's full-history fallback, but incurs a new WebSocket handshake and may have different first-request cache/latency characteristics. While the lease is warm, direct and gateway ChatGPT WebSocket upstream payloads are transport-equivalent.

After intentional boundary normalization, the central provider receives the same model-semantic `llm.Request` as a direct provider: model and session identity; system/developer/user/assistant/tool history; tool definitions and choices; tool-call and tool-result continuation data; cache anchors; reasoning summaries, encrypted reasoning, and opaque provider replay; search controls; Responses options; sampling, output-token, service-tier, and ephemeral/continuation controls. This equivalence has deliberately narrow exceptions:

- `WorkingDir` is replaced with a fresh empty gateway-owned run directory.
- Satellite image/file paths are removed while inline image/file data is retained. Tool-result diff/image paths, legacy display strings, formatted `ToolInfo`, provider tool-activity display records, skill-activation provenance, and persisted UI identity/segment fields are omitted. Expanded developer instructions, actual tool arguments/results, multimodal result data, caller provenance, thought signatures, reasoning, and provider replay remain.
- Approval-only transcripts/roles, request-scoped execution filters (`AllowedTools`), satellite engine controls (`MaxTurns` and `ToolMap`), and debug flags are local and do not cross the provider boundary. The satellite engine consumes these before or around provider execution; omitting them cannot change the upstream model payload.

Run the gateway only on a trusted private network. For traffic that is not already protected by a private overlay, service mesh, or TLS reverse proxy, configure the built-in TLS listener with both `--tls-cert` and `--tls-key`. Bearer credentials authenticate access but plaintext HTTP does not encrypt them.

## Start a gateway

Use the normal central term-llm config for provider credentials. Authenticate interactive providers on the gateway host before starting the service; HTTP handlers never initiate an interactive OAuth flow.

Create a client:

```bash
term-llm gateway client add jarvis \
  --allow-provider anthropic \
  --allow-provider openai \
  --allow-search \
  --max-concurrent-inference 2
```

The token is shown once. Client records contain only a SHA-256 token hash. State defaults to `~/.config/term-llm/gateway`; override it for a service volume:

```bash
term-llm gateway serve \
  --listen 0.0.0.0:8787 \
  --state-dir /var/lib/term-llm-gateway
```

Successful sessionful satellite WebSocket providers are retained for 30 seconds by default. Tune this with `--provider-session-idle-timeout DURATION`; `0` disables reuse, while negative durations are rejected. This setting does not make `/v1/responses` stateful.

Gateway state contains:

```text
clients.json               # client IDs, token hashes, policy, revocation
clients.enrollments.json   # hashed one-use enrollment tokens, expiry, policy, use time
state.key                  # 32-byte provider-state sealing key
usage.jsonl                # attributed request usage/outcomes
runs/                      # state-owned ephemeral run directories; scavenged at startup
```

Back up `state.key` with the client database. Losing it safely invalidates existing opaque provider resume state but does not expose provider credentials.

Manage clients:

```bash
term-llm gateway client list
term-llm gateway client revoke jarvis
term-llm gateway usage --client jarvis
```

Client additions and revocations are written atomically. Active client names are unique so name-based revocation is unambiguous. To rotate a credential, revoke the existing ID or name, then run `gateway client add` again with the same name; the old token stops authenticating before the replacement is issued. A running gateway reloads the durable client file on each authentication decision, so a management CLI revocation takes effect on the next request without a polling interval or restart.

Client policy also persists independent inference, search, and fetch controls. Safe defaults are two concurrent inference requests, two concurrent search/fetch requests, and 30 search/fetch requests per minute with a burst of five. Configure them with `--max-concurrent-inference`, `--search-rate`, `--max-concurrent-search`, `--fetch-rate`, and `--max-concurrent-fetch`. Permits are client-scoped and released when requests finish or are canceled.

### Enrollment

For bootstrap automation, have the gateway operator create a persisted, single-use token. Enrollment tokens default to 15 minutes, are stored only as hashes, are atomically marked used, and are bound to the requested client name and policy:

```bash
term-llm gateway client add jarvis --enroll \
  --allow-provider anthropic \
  --allow-model 'anthropic:claude-sonnet-*' \
  --max-concurrent-inference 2 \
  --enroll-ttl 15m
```

Enrollment refuses unrestricted policies: at least one `--allow-provider` or `--allow-model` is required. Search, fetch, and CLI access remain off unless explicitly enabled. The default enrollment command is intentionally quiet about the new client token. It atomically updates `$XDG_CONFIG_HOME/term-llm/config.yaml` (or `~/.config/term-llm/config.yaml`) and writes the credential separately to a mode-`0600` `gateway-token` file:

```bash
term-llm gateway enroll https://gateway:8787 tlge1_REDACTED --name jarvis
```

Use `--token-file PATH` to choose the credential path. For scripts that manage configuration themselves, use `--write-config=false --token-file PATH`. `--print-only` performs no writes and explicitly prints minimal YAML containing the client token; this is the only enrollment mode that prints that token. Reusing an enrollment token, using it after expiry, or requesting a different name fails. Direct `gateway client add` remains available when an operator can securely transfer the final client token.

## Discourse AI model setup

The Responses edge is intended for Discourse AI's existing OpenAI Responses client. In **Admin → Plugins → Discourse AI → LLMs**, create a model with:

| Discourse field | Value |
|---|---|
| Provider | **OpenAI** (`open_ai`; do not select OpenRouter even when the gateway routes to OpenRouter) |
| API endpoint / URL | `https://gateway.example/v1/responses` — include the path exactly |
| API key | the gateway client token printed once by `term-llm gateway client add discourse ...` |
| Model name | a namespaced gateway ID such as `chatgpt/gpt-5.6-sol` or `openrouter/moonshotai/kimi-k2` |

Discourse selects its Responses dialect only when the provider is OpenAI (or Azure) and the URL contains `/v1/responses`. The model namespace is split on its **first** slash, so provider models may contain additional slashes. Create a dedicated token with narrow policy, for example:

```bash
term-llm gateway client add discourse \
  --allow-provider chatgpt \
  --allow-model 'chatgpt:gpt-5.6-sol' \
  --max-concurrent-inference 4
```

Set these Discourse custom provider parameters where relevant:

- `disable_native_tools: true` — required when agents may request Discourse tools. The gateway accepts Responses `type: function` tools only; it deliberately rejects hosted `web_search`, file-search, computer-use, MCP, and other gateway-side tool types.
- `reasoning_effort`: one of the efforts advertised for that namespaced model (for example `low`, `medium`, `high`, or `xhigh`).
- `service_tier`: `auto`, `flex`, or `priority` when supported by the selected provider/model.
- `disable_temperature: true` and/or `disable_top_p: true` for reasoning models that reject sampling controls. Discourse already removes both when `reasoning_effort` is configured.

`GET /v1/models` uses the same bearer token and returns only configured models allowed by both server and client policy. Model IDs are always namespaced. Dynamic providers may route policy-allowed unlisted model IDs, but an unlisted ID cannot be advertised by the list endpoint.

A Discourse-shaped streaming probe is:

```bash
curl --no-buffer https://gateway.example/v1/responses \
  -H "Authorization: Bearer $TERM_LLM_GATEWAY_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @- <<'JSON'
{
  "model": "chatgpt/gpt-5.6-sol",
  "input": [
    {"role":"developer","content":"Answer concisely."},
    {"role":"user","content":[{"type":"input_text","text":"Say hello."}]}
  ],
  "max_output_tokens": 256,
  "reasoning": {"summary":"auto","effort":"medium"},
  "include": ["reasoning.encrypted_content"],
  "stream": true
}
JSON
```

Each SSE record is a single `data: {json}` line followed by a blank line. A `[DONE]` sentinel is not required. The edge emits Responses-native created/in-progress, reasoning summary, text, function-call argument, output-item, and completed events. `response.completed.response.output` contains the complete reasoning, message, and function items used by Discourse for stateless replay. Client disconnect cancels the provider request and records a `canceled` gateway usage outcome.

### Responses compatibility and deliberate exclusions

Supported request semantics are: string or item-array `input`; developer/user/assistant messages; `input_text`, `output_text`, base64 data-URL `input_image`, and base64 data-URL `input_file`; encrypted reasoning replay; stateless `function_call` plus `function_call_output`; flat function tools; `none`/`auto`/`required`/named function tool choice; max output tokens; reasoning effort with `summary: auto`; temperature; top-p; service tier; parallel tool calls; streaming; usage; and harmless metadata/cache identity fields that do not alter execution.

The edge clearly rejects semantics it cannot preserve: `previous_response_id` and conversation objects (send complete stateless input), background execution, automatic truncation, structured `text.format` output, nonzero logprobs, max hosted-tool-call limits, non-function/hosted tools, unsupported include values, and unknown top-level fields. It does not expose private `/g1` cancellation or sealed `ProviderState` as OpenAI protocol fields. Provider `reasoning.encrypted_content` is replay data sent only back to the selected provider; it is never accepted as or opened as gateway-sealed local provider state.

## Satellite configuration

The minimal configuration is:

```yaml
gateway:
  url: http://gateway:8787
  token_env: TERM_LLM_GATEWAY_TOKEN
```

A token can instead be supplied with `gateway.token` or a mode-`0600` `gateway.token_file`. Resolution precedence is token, token file, then environment.

Full client controls:

```yaml
gateway:
  url: http://gateway:8787
  token_file: /run/secrets/term_llm_gateway_token
  required: true
  local_providers: [ollama, laptop-vllm]
  search: true
  fetch: true
  catalog_ttl: 15m
  connect_timeout: 2s
  response_timeout: 5s
  idle_timeout: 5m
  tool_timeout: 10m
```

Gateway catalogs use live provider `ListModels` results where supported and fall back to configured/curated metadata or the last successful provider entry on transient refresh failures. Strict configured entries are available stale-first while each provider refreshes independently in the background; inference for one provider never waits for listing an unrelated provider. The server reloads provider config on its bounded `--catalog-ttl`. Known dynamic aggregators can explicitly permit unlisted models; set `providers.<name>.allow_unlisted_models: false` to force exact catalog membership, or `true` for another intentionally dynamic provider. The hidden `debug` provider has its own catalog type and always defaults to strict configured models rather than inheriting OpenAI-compatible behavior. Provider/model allow and deny policy is always enforced, including for an allowed unlisted model.

Satellite catalogs are memoized per gateway identity in-process, coalesced across concurrent callers, and cached under the XDG cache directory with mode-`0600` files. Refreshes use ETags, short connect/response bounds, and stale disk fallback. Shell completion is cache-only and never waits for a gateway network request.

Routing precedence is deterministic:

1. `gateway.local_providers`
2. an explicit local `providers.<name>` block
3. the configured gateway

Once `gateway.url` is set, providers advertised by the gateway—including `debug`—route remotely from normal CLI commands. They remain local only when named in `gateway.local_providers` or given an explicit local `providers.<name>` block. Providers not explicitly local fail closed if the gateway or catalog is unavailable; they never silently fall back to local credentials or built-ins. `gateway.required` additionally rejects a configuration that requests a gateway without setting `gateway.url`. With no `gateway.url`, behavior is unchanged.

`gateway.search` and `gateway.fetch` default to `true` in a minimal gateway block. An explicit `false` is preserved and selects the ordinary local search/fetch configuration. When remote routing is enabled, gateway construction or outages produce a legible tool error rather than silently falling back to DuckDuckGo/Jina or removing `read_url`.

## CLI providers are opt-in

`claude-bin`, `grok-bin`, `cursor-bin`, and `gemini-cli` execute programs and use credentials on the gateway host. They are denied by default in two places:

- the server must start with `--allow-cli`
- the client must be created with `gateway client add NAME --allow-cli`

Both gates are required. CLI providers always receive a gateway-created empty temporary working directory. Their MCP/tool bridge calls are sent back to the originating authenticated satellite; they never run against a gateway-side copy of satellite tools or files.

Opting in still gives the CLI provider access to its gateway-side account and whatever the CLI itself can reach from the gateway container. Use a dedicated Unix user/container, minimal environment, read-only root filesystem where practical, and narrow per-client provider/model policy.

## Container/Jarvis migration

For an existing Jarvis or `term-llm contain` satellite:

1. Move provider API keys, OAuth stores, CLI homes, and search credentials to the gateway service.
2. Create one gateway client per container. Do not share tokens between satellites; attribution and revocation are client-scoped.
3. Replace provider secrets in the satellite with the `gateway` block and token secret.
4. Keep agent files, memory, jobs, session database, project mounts, and tool approvals in the satellite volume.
5. Add intentionally local providers such as an in-container Ollama instance to `gateway.local_providers`.
6. Verify `term-llm models --provider <name>` and a no-tool prompt, then test an approved local tool call.
7. Revoke old credentials from the satellite after validation.

See `ops/gateway-compose.yaml` for a private Docker network example. Both services use the built-in `gateway health URL` probe, and the satellite waits for a healthy gateway. The gateway has no published host port; only the satellite UI is exposed. CI parses the Compose YAML and verifies this health gating without requiring a Docker daemon.

## Operational checks

```bash
curl http://gateway:8787/g1/health
term-llm models --provider anthropic
```

`/g1/health` intentionally returns only status and protocol version. Catalog/inference/search/fetch/run endpoints require the client bearer token and protocol version negotiation.

Use `term-llm gateway usage` (optionally `--client` or `--json`) to inspect `usage.jsonl`, including failures, explicit `canceled` outcomes, and successful requests. Satellite-local term-llm usage remains visible with `tracked_externally_by: gateway`; aggregate local usage excludes that copy by default to avoid double-counting the gateway record. `term-llm usage --include-external` includes those external copies in either the all-provider or `--provider term-llm` view. Provider failures sent to satellites carry safe structured codes for API-key/OAuth authentication, rate limiting, context limits, invalid models/requests, and upstream failures, with a gateway/provider-specific action. Raw provider details are logged only on the gateway host and never include upstream bodies in satellite errors.
