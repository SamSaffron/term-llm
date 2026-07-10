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
- feature-specific blocks such as `image`, `audio`, `music`, `embed`, `search`, `sessions`, `file_tracking`, `tools`, and `skills`

## Example

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
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
  # smart: update terminal tab titles and tmux window names.
  # basic: only emit the terminal/tab OSC title.
  # off: do not touch terminal/window titles at all.
  terminal_title: smart

approval:
  # New chat sessions start in prompt mode by default.
  # Set to auto to have unmatched shell commands reviewed by guardian first.
  default_mode: prompt

guardian:
  # Optional: override the provider/model used for auto approval review.
  # If omitted, the current chat provider/model is used.
  # provider: anthropic
  # model: claude-sonnet-4-6
  # policy_path: ~/.config/term-llm/guardian-policy.md
  # Review timeout in seconds. Defaults to 90.
  # timeout_seconds: 90

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

## Approval modes

Tool access normally uses **prompt** mode: if a command or path is outside the configured allowlists, term-llm asks before proceeding.

Interactive chat also supports **auto** mode for shell commands. In auto mode, deterministic approvals still run first (`--shell-allow`, session approvals, project approvals, and exact guardian cache hits). If a shell command is still unmatched, term-llm asks a guardian reviewer model to evaluate the exact command, working directory, transcript evidence, and existing read/write/tool approval context. Guardian approvals are cached only for the exact command and working directory. Guardian denials or review failures fall back to the human prompt in interactive chat and fail closed in headless runs.

You can enable auto mode per run:

```bash
term-llm chat --auto
```

Or make new chat and web/serve sessions start in auto mode:

```yaml
approval:
  default_mode: auto
```

Explicit flags still win. `--yolo` starts yolo mode, `--auto` starts auto mode, and resumed sessions restore their saved `prompt`/`auto` mode. Stored `yolo` is not restored on cold resume for safety.

Auto mode is intentionally narrower than yolo:

- `prompt`: ask before unapproved tool actions.
- `auto`: guardian-review unmatched shell commands; other approvals still prompt.
- `yolo`: auto-approve tool actions without prompting.

Configure guardian review with:

```yaml
guardian:
  provider: anthropic       # optional override
  model: claude-sonnet-4-6  # optional override
  policy_path: ~/.config/term-llm/guardian-policy.md # optional custom policy
  timeout_seconds: 90        # optional; default 90
```

If `guardian.provider` is set and `guardian.model` is omitted, term-llm uses that provider's configured model/fast model instead of accidentally mixing it with the chat provider's model.

> Privacy note: guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If you set `guardian.provider` to a different provider than your chat provider, that evidence is sent to the guardian provider as well. Leave `guardian.provider` unset if you do not want approval evidence routed to an additional provider.

## Per-command overrides

Each command can override provider and model independently of the global default.

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
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

## Chat terminal titles

`chat.terminal_title` controls whether interactive chat updates terminal titles:

- `smart` (default): update the terminal/tab title and, inside tmux, rename the current tmux window.
- `basic`: emit only the terminal/tab title escape; tmux window names and Ghostty progress are not managed.
- `off`: emit no title/progress escape sequences and do not rename tmux windows.

```yaml
chat:
  terminal_title: smart
```

`chat.terminal_title_format` optionally customizes the text used for both the terminal/tab title and smart tmux window title. Leave it empty for the built-in smart format (`agent · task · model`, with streaming/attention variants).

```yaml
chat:
  terminal_title: smart
  terminal_title_format: "[{{env.DOCKER_CONTAINER_NAME}}] {{agent}} · {{task}} · {{model}}"
```

Supported placeholders:

- `{{title}}` - the built-in title for the current state.
- `{{stable_title}}` - built-in title without elapsed streaming seconds; useful for low-churn window names.
- `{{agent}}` - current agent name.
- `{{task}}` - session/task title, generated from the conversation when available.
- `{{model}}` - display model name.
- `{{phase}}` - current phase, such as `Thinking` or `Responding`.
- `{{state}}` - `idle`, `streaming`, or `attention`.
- `{{activity}}` - model name while idle, streaming activity while streaming, or `attention` while waiting for input.
- `{{elapsed}}` - streaming elapsed time such as `12s` when available.
- `{{attention}}` / `{{attention_marker}}` - `‼` when approval/ask-user/handover attention is needed, otherwise empty.
- `{{env "NAME"}}` or `{{env.NAME}}` - environment variable from the chat process. Use this sparingly: terminal titles can be visible to your terminal, window manager, tmux, screen sharing, and logs, so do not include secrets.

The format is a Go `text/template`, so pipelines work too. A small `default` helper returns its first argument when the piped value is empty:

```yaml
chat:
  terminal_title_format: "[{{env \"DOCKER_CONTAINER_NAME\" | default \"host\"}}] {{title}}"
```

Title text is sanitized before emission: control characters are removed, whitespace is collapsed, and very long results are truncated.

Ghostty note: Ghostty honors `OSC 2` title updates, but Ghostty can also override or ignore program-supplied titles in a few cases. In `smart` mode, term-llm also emits Ghostty's `OSC 9;4` indeterminate progress indicator while a chat turn is running and clears it when the turn settles. When running inside tmux, term-llm wraps this progress sequence in tmux passthrough so Ghostty can still see it; tmux 3.3+ may require `set -g allow-passthrough on` in `~/.tmux.conf` for passthrough sequences to reach Ghostty.

Ghostty title caveats:

- A Ghostty config `title = ...` forces a fixed title and ignores title escape sequences.
- A configured `title-command` takes precedence over title escape sequences.
- A title set with Ghostty's title prompt/actions (for example `prompt_surface_title`, `prompt_tab_title`, `set_surface_title`, or `set_tab_title`) can override terminal-supplied titles until cleared.
- Ghostty shell integration's `title` feature rewrites the title before/after shell commands. If you want manual/program titles to persist at the shell prompt, set this in Ghostty config and reload/restart the shell:

```ini
shell-integration-features = no-title
```

To debug Ghostty title handling outside term-llm, run this from a Ghostty shell:

```sh
printf '\033]2;ghostty title test\a'; sleep 10
```

To debug the Ghostty header progress indicator:

```sh
printf '\033]9;4;3\a'; sleep 10; printf '\033]9;4;0\a'
```

If the title changes only during `sleep` and resets afterward, Ghostty shell integration is overwriting it at the prompt. If it never changes, check for a fixed `title`, `title-command`, or a manual surface/tab title override.

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
  strip_image_base64: false
```

Use this to control whether sessions are persisted, how long they are kept, and where the SQLite database lives. By default, uploaded image base64 is kept in the DB for portability; set `strip_image_base64: true` to store only image paths/metadata when a local `ImagePath` exists, reducing DB size at the cost of requiring the uploads directory to move with the database.

## File change tracking config

```yaml
file_tracking:
  enabled: false
  max_file_bytes: 2097152 # 2 MiB per-file content cap
  max_session_bytes: 104857600 # 100 MiB retained content per session
  max_total_bytes: 1073741824 # 1 GiB whole-database cap across sessions
  path: "" # optional DB path override
```

Opt-in. When enabled, term-llm records the before/after contents of files that agent tools create, modify, or delete, so the web UI can show a live per-session diff sidebar.

Enable it with:

```bash
term-llm config set file_tracking.enabled true
```

or by adding the YAML above to your config file.

**Privacy note:** this persists actual file contents (not just paths) to a local SQLite database at `~/.local/share/term-llm/file_history.db`, separate from `sessions.db`. Contents are gzip-compressed and content-addressed. Files larger than `max_file_bytes`, binary files, and changes beyond the per-session budget are recorded as metadata only ("content not retained"). History for deleted sessions is swept on startup, following `sessions.max_age_days`; if the database still exceeds `max_total_bytes`, the least recently changed sessions' history is pruned until it fits.

Shell-made changes are tracked best-effort: commands that declare an `affected_paths` hint are snapshotted precisely; otherwise term-llm relies on `git status` (when inside a repository) and re-checking files the session already touched. Broad scripts writing to non-git directories without a hint may not appear in the diff sidebar.

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
    model: gpt-5.6-sol
    service_tier: fast

  chatgpt:
    model: gpt-5.6-sol-medium
    service_tier: priority
```

In chat mode, `/fast` toggles this service tier for the current session. It does not rewrite your config file.

## GPT-5.6 advanced Responses controls

The `responses` provider block configures execution features specific to **OpenAI API GPT-5.6**. It is separate from the top-level `reasoning` block, which controls whether reasoning is displayed, persisted, or exported.

```yaml
providers:
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
    responses:
      reasoning_mode: standard       # standard or pro
      reasoning_context: auto        # auto, current_turn, or all_turns
      multi_agent:
        enabled: true
        max_concurrent_subagents: 3  # defaults to 3 when enabled
      programmatic_tool_calling:
        enabled: true
        tools: [read_file, grep]
      prompt_cache:
        mode: explicit               # implicit or explicit
        ttl: 30m                     # currently the only supported TTL
```

Rules and caveats:

- These controls are accepted only by the built-in `openai` provider with `gpt-5.6-sol`, `gpt-5.6-terra`, or `gpt-5.6-luna`. term-llm rejects them for older models, custom/OpenAI-compatible providers, and ChatGPT OAuth.
- `reasoning_mode: pro` is public-API Pro mode. ChatGPT's `ultra` is a separate effort and is not configured here.
- `reasoning_context` accepts only `auto`, `current_turn`, or `all_turns`.
- Enabling multi-agent without a concurrency value uses `3`. Multi-agent requests omit the normal reasoning-summary request and send the required beta header.
- Programmatic tool calling requires at least one listed tool, and every name must also be present in that request's tool definitions.
- Prompt-cache `mode` accepts `implicit` or `explicit`; the only supported explicit TTL is `30m`. Explicit content breakpoints are available through the Responses API as `prompt_cache_breakpoint: {"mode":"explicit"}`.
- Request-level options override populated provider defaults. Ephemeral internal requests, such as title and helper calls, do not inherit provider advanced defaults unless the caller explicitly supplies options.

In terminal chat, `/pro on`, `/pro off`, and `/pro status` set the session's reasoning mode. The setting is persisted with the session and automatically disabled if you switch to a provider/model that does not support it.

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
    model: gpt-5.6-sol
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

Use `type: vllm` for vLLM servers that should receive reasoning-model chat-template controls. It uses the same `base_url`, `url`, `api_key`, `context_window`, and `max_output_tokens` fields as `openai_compatible`, but maps term-llm reasoning effort suffixes into vLLM request fields for supported model families:

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

The suffix is stripped before the model name is sent upstream. For example `cdck_qwen-high` still sends `Qwen/Qwen3.5-122B-A10B` as the model and adds `chat_template_kwargs.enable_thinking=true` plus `thinking_token_budget=10000`. Plain/default Qwen requests send `enable_thinking=false` and omit `thinking_token_budget`; budgeted Qwen efforts require a vLLM server configured to accept `thinking_token_budget` (recent vLLM requires `--reasoning-config`).

DeepSeek served through vLLM uses a different official shape. If the model name contains `deepseek`, term-llm auto-selects DeepSeek controls; for aliased or mistitled deployments, set `vllm_thinking_param: thinking`:

```yaml
providers:
  cdck_deepseek:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: ds31
    vllm_thinking_param: thinking
```

DeepSeek efforts map to `chat_template_kwargs.thinking` and nested `chat_template_kwargs.reasoning_effort`: default/off sends `thinking=false`, `low`/`medium`/`high` send `thinking=true, reasoning_effort=high`, and `xhigh`/`max` send `thinking=true, reasoning_effort=max`. DeepSeek requests do not send `thinking_token_budget`.

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
