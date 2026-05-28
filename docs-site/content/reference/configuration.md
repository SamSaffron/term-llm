---
title: "Configuration"
weight: 2
description: "Inspect, edit, and understand the main config file, provider settings, search configuration, and per-command overrides."
kicker: "Config"
featured: true
next:
  label: Providers and models
  url: /reference/providers-and-models/
---
## Configuration commands

```bash
term-llm config
term-llm config edit
term-llm config path
term-llm config get default_provider
term-llm config set default_provider zen
term-llm config reset
```

The main config file lives at:

```text
~/.config/term-llm/config.yaml
```

## Configuration shape

A typical config has a few major parts:

- `default_provider` for the global LLM default
- `providers` for model-specific credentials and routing
- per-command blocks such as `exec`, `ask`, and `edit`
- feature-specific blocks such as `image`, `audio`, `music`, `embed`, `search`, `sessions`, `tools`, and `skills`

## Example

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.2
    credentials: codex
    # WebSocket transport is enabled by default for built-in OpenAI.
    # Set false to force HTTP/SSE.
    use_websocket: true

  xai:
    model: grok-4-1-fast

  nearai:
    model: zai-org/GLM-5.1-FP8
    fast_model: Qwen/Qwen3.6-35B-A3B-FP8

  sambanova:
    model: gpt-oss-120b
    fast_model: Meta-Llama-3.3-70B-Instruct

  claude-bin:
    model: opus
    env:
      IS_SANDBOX: "1"

  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm

exec:
  suggestions: 3
  instructions: |
    I use Arch Linux with zsh.
    I prefer ripgrep over grep and fd over find.

ask:
  max_turns: 50
  instructions: |
    Be concise. I'm an experienced developer.

chat:
  max_turns: 200

edit:
  model: gpt-5.2-codex
  diff_format: auto

search:
  provider: exa_mcp
  fetch_provider: jina

  exa_mcp:
    url: https://mcp.exa.ai/mcp # optional; this is the default
    api_key: ${EXA_API_KEY} # optional

tools:
  max_tool_output_chars: 20000
```

## Per-command overrides

Each command can override provider and model independently of the global default.

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6
  openai:
    model: gpt-5.2
  zen:
    model: glm-4.7-free

exec:
  provider: zen
  model: glm-4.7-free

ask:
  model: claude-opus-4

edit:
  provider: openai
  model: gpt-5.2-codex
```

Precedence is:

1. CLI flag such as `--provider openai:gpt-5.2`
2. per-command config such as `exec.provider` or `ask.model`
3. global provider selection via `default_provider` and `providers.<name>.model`

## Agentic turn limits

Agentic commands can make multiple provider calls while they execute tools and feed results back to the model. `max_turns` caps that loop.

Defaults:

- `ask.max_turns`: `50`
- `exec` CLI flag default: `50`
- `chat.max_turns`: `200`
- Agent YAML `max_turns` overrides command/config defaults when an agent is selected.
- A CLI `--max-turns N` flag overrides both config and agent YAML.

```yaml
ask:
  max_turns: 50

chat:
  max_turns: 200
```

## Parallel tool execution

Models may request many independent tool calls in a single turn, such as several `read_file`, `grep`, or `glob` calls. term-llm executes independent tool calls concurrently when parallel tool calls are enabled by the provider/request, but caps one model turn at **20 concurrently running tool calls**. Additional tool calls from the same turn are queued and run as earlier calls finish.

This is a built-in safety limit rather than a config option today. It preserves useful batching while preventing a single response from spawning an unbounded number of shells, greps, reads, or subagents at once.

## Reasoning and thinking display

Reasoning display controls how provider-marked thinking/summary content is shown in term-llm. It is separate from provider reasoning effort suffixes such as `openai:gpt-5.2-high`, `anthropic:...-thinking`, or `vllm` provider `-high`.

By default, term-llm shows display-safe provider summaries and non-encrypted provider thinking as collapsed thought blocks in interactive chat:

- Generic provider thinking renders as `▸ Thinking...`.
- Provider/summary titles render as `▸ Thought: <title>`.
- Expanding a block shows the body; encrypted reasoning/signatures are never displayed.
- In chat, `Ctrl+E` toggles thought detail globally and clicking a thought header toggles that block.
- Ctrl+O inspector shows non-encrypted reasoning details for saved messages.

Default policy:

```yaml
reasoning:
  display: auto                  # auto => collapsed
  source: summary_or_provider_safe
  status: title
  history: collapsed
  export: ask
  raw: false
  max_summary_chars: 12000
  max_raw_chars: 20000
  extract_titles: true
  hidden_label: Thinking...
  persist_summaries: true
```

Important options:

| Field | Values | Meaning |
|---|---|---|
| `display` | `auto`, `off`, `status`, `collapsed`, `expanded`, `raw` | Interactive display mode. `raw` still requires `raw: true`; otherwise it falls back to collapsed. |
| `source` | `summary_only`, `summary_or_provider_safe`, `all` | Which provider reasoning sources interactive UI may show. Raw export/replay requires `all`. |
| `status` | `none`, `generic`, `title`, `summary` | How reasoning affects the live status/spinner text. |
| `history` | `none`, `collapsed`, `expanded`, `transcript_only` | Whether saved/streamed thought blocks are visible in chat history. |
| `export` | `never`, `ask`, `summaries`, `raw` | What session export may include. Raw export also requires `raw: true` and `source: all`. |
| `raw` | boolean | Explicit safety gate for raw reasoning display/export. |
| `hidden_label` | string | Label for untitled collapsed blocks, default `Thinking...`. |

Per-surface overrides inherit the top-level reasoning policy:

```yaml
reasoning:
  display: collapsed
  chat:
    display: expanded
  ask:
    status: title
    history: none
  serve:
    display: off
```

For local debugging only, `TERM_LLM_SHOW_RAW_REASONING=1` forces `display: raw`, `source: all`, and `raw: true` for the resolved surface.

## Sessions config

```yaml
sessions:
  enabled: true
  max_age_days: 0
  max_count: 0
  path: ""
```

Use this to control whether sessions are persisted, how long they are kept, and where the SQLite database lives.

## Search config

```yaml
search:
  provider: exa_mcp
  fetch_provider: jina
  force_external: false

  exa_mcp:
    url: https://mcp.exa.ai/mcp # optional; this is the default
    api_key: ${EXA_API_KEY} # optional, raises free-tier limits

  perplexity:
    api_key: ${PERPLEXITY_API_KEY}

  exa:
    api_key: ${EXA_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}
```

Defaults are `provider: exa_mcp` and `fetch_provider: jina`: external search uses Exa's remote MCP server, while `read_url` uses Jina Reader. Set `fetch_provider: exa_mcp` to fetch pages through Exa MCP as well, or `fetch_provider: none` to omit the external `read_url` tool.

Search is large enough to deserve its own page; see [Search](/guides/search/).

## Image, audio, music, transcription, and embedding config

```yaml
image:
  provider: gemini
  output_dir: ~/Pictures/term-llm

audio:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: ${VENICE_API_KEY}
    model: tts-kokoro
    voice: af_sky
    format: mp3

music:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: ${VENICE_API_KEY}
    model: elevenlabs-sound-effects-v2
    format: mp3
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: music_v1
    format: mp3_44100_128

transcription:
  provider: venice
  venice:
    api_key: ${VENICE_API_KEY}
    model: nvidia/parakeet-tdt-0.6b-v3
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: scribe_v2

embed:
  provider: gemini
```

Each feature block can hold provider-specific credentials and defaults. The image, audio, music, transcription, and embedding providers are independent of the main text provider.

## Provider-specific environment overrides

Providers that shell out to local CLIs can accept extra subprocess environment variables via `providers.<name>.env`.

For `claude-bin`, term-llm also disables Claude Code hooks by default so user-level Claude automation does not leak into inference sessions. Set `providers.claude-bin.enable_hooks: true` if you explicitly want Claude Code hooks to run.

Example for Claude Code when term-llm runs inside a trusted sandboxed container:

```yaml
providers:
  claude-bin:
    model: opus
    env:
      IS_SANDBOX: "1"
      # Generate a long-lived token with: claude setup-token
      # Useful in CI or headless environments where interactive login isn't possible
      CLAUDE_CODE_OAUTH_TOKEN: "your-oauth-token-here"
    # Optional: re-enable Claude Code hooks for this provider
    # enable_hooks: true
```

`providers.<name>.env` values support the same resolution rules as other deferred config values:

- `file://path` → trimmed file contents
- `file://path#json.path` → JSON field extracted from the file
- `op://...` → 1Password secret lookup
- `$()` → command output
- `${VAR}` / `$VAR` → environment variable expansion

This is passed only to the provider subprocess. It does not mutate your parent shell environment.

## Provider service tier

Built-in `openai` and `chatgpt` text providers support the Responses API `service_tier` field. Omit `service_tier` to send no service tier. Set it to `fast` (or the API value `priority`) to request fast/priority service for supported models and accounts:

```yaml
providers:
  openai:
    model: gpt-5.4
    service_tier: fast

  chatgpt:
    model: gpt-5.5-medium
    service_tier: priority
```

In chat mode, `/fast` toggles this service tier for the current session. It does not rewrite your config file.

## Provider file upload policy

Provider configs can override which MIME types may be forwarded as native file/document inputs. This matters for the web/API `input_file` path: term-llm saves uploads locally first, then either sends a native file part, embeds text-like files as prompt text, or falls back to a local marker for unsupported binaries.

Built-in defaults are conservative:

- `openai`, `chatgpt`, and `copilot` allow OpenAI Responses-style document/spreadsheet/text MIME types natively by default.
- Providers without an implemented native file path do not forward native file parts; they use text fallback/marker behavior instead.
- Text-like files (`txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`, and common code files) can still be embedded as ordinary text on providers without native file support, wrapped in explicit begin/end file markers.

Example custom policy:

```yaml
providers:
  openai:
    model: gpt-5.4
    file_upload:
      native_mime_types:
        - application/pdf
        - text/plain
        - text/markdown
        - text/csv
        - application/json
      max_native_bytes: 20971520
      text_embed_mime_types:
        - text/plain
        - text/markdown
        - text/csv
        - application/json
      max_text_embed_bytes: 20971520
```

To disable native forwarding while keeping text fallback available:

```yaml
providers:
  openai:
    file_upload:
      native_mime_types: []
```

The server still enforces its upload limits (10 attachments, 20 MB decoded per attachment, and 50 MB total JSON request body). Provider-native limits may be lower or higher; if a provider rejects a native file type, remove it from `native_mime_types` so term-llm falls back to text/marker behavior.

## Provider WebSocket transport

Built-in `openai` and `chatgpt` text providers use the Responses WebSocket transport by default for lower-latency agent/tool loops. The WebSocket path keeps a persistent connection and, when safe, continues turns with `previous_response_id` plus only the new user/tool input. If the WebSocket connect/write step fails, term-llm falls back to HTTP/SSE; if a WebSocket continuation is rejected because the prior response state is unavailable, it retries that turn once with full state.

Disable it per provider if you need to force HTTP/SSE:

```yaml
providers:
  openai:
    use_websocket: false
  chatgpt:
    use_websocket: false
```

OpenAI-compatible providers (`type: openai_compatible`, including local/self-hosted endpoints and OpenRouter-style compatible APIs) do **not** enable WebSockets by default. They continue to use HTTP/SSE unless explicitly supported and wired by that provider.

## vLLM providers

Use `type: vllm` for vLLM servers that should receive Qwen thinking controls. It uses the same `base_url`, `url`, `api_key`, `context_window`, and `max_output_tokens` fields as `openai_compatible`, but maps term-llm reasoning effort suffixes into vLLM request fields:

```yaml
providers:
  cdck_qwen:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: Qwen/Qwen3.5-122B-A10B
    api_key: ${CDCK_QWEN_API_KEY}
    context_window: 200000
    max_output_tokens: 50000
```

```bash
term-llm ask -p cdck_qwen       "quick answer" # thinking disabled by default
term-llm ask -p cdck_qwen-low   "think a bit"  # budget 1024
term-llm ask -p cdck_qwen-high  "think hard"   # budget 10000
```

The suffix is stripped before the model name is sent upstream. For example `cdck_qwen-high` still sends `Qwen/Qwen3.5-122B-A10B` as the model and adds `chat_template_kwargs.enable_thinking=true` plus `thinking_token_budget=10000`.

term-llm persists streamed reasoning and replays it as assistant `reasoning` on future vLLM turns so vLLM's chat template and prefix cache can see the same prior reasoning. vLLM may still report `reasoning_tokens: 0` in usage metadata even when reasoning text is present; that is a vLLM accounting limitation.

## Dynamic secrets and endpoints

term-llm supports dynamic resolution for some config values:

- `op://...` for 1Password secret references
- `srv://...` for DNS SRV-based endpoint discovery
- `$()` for command-based resolution

Example:

```yaml
providers:
  production-llm:
    type: vllm
    model: Qwen/Qwen3-30B-A3B
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

These values are resolved lazily when term-llm actually needs them.

## WebRTC direct routing config

```yaml
serve:
  webrtc:
    enabled: true
    signaling_url: https://signal.example.com/webrtc
    token: your-signaling-token
    stun_urls:
      - stun:stun.l.google.com:19302
    max_conns: 10
```

These values match the `--webrtc-*` CLI flags. See the [WebRTC direct routing](/guides/webrtc-direct-routing/) guide for full details.

## Skills config

```yaml
skills:
  enabled: true
  auto_invoke: true
  metadata_budget_tokens: 8000
  max_visible_skills: 50
  include_project_skills: true
  include_ecosystem_paths: true
  always_enabled: [git, code-review]
  never_auto: [expensive-api-skill]
```

Controls the skills system: portable instruction bundles that inject task-specific context into the system prompt. Skills are disabled by default; set `enabled: true` to allow auto-invocation, or use `--skills` on any command for one-off activation. See [Skills](/guides/skills/) for the full guide.

## Diagnostics

```yaml
diagnostics:
  enabled: true
```

When edit retries fail, diagnostics can capture prompts, partial responses, and failure context for inspection.

## Related pages

- [Providers and models](/reference/providers-and-models/)
- [Search](/guides/search/)
- [Sessions](/reference/sessions/)
- [Skills](/guides/skills/)
- [Text embeddings](/guides/text-embeddings/)
