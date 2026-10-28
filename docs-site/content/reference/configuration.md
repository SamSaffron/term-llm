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
term-llm config set default_provider openrouter
term-llm config reset
term-llm config completion fish
```

The main config file is `$XDG_CONFIG_HOME/term-llm/config.yaml`. Without `XDG_CONFIG_HOME`, it lives at:

```text
~/.config/term-llm/config.yaml
```

## Configuration shape

A typical config has a few major parts:

- `default_provider` for the global LLM default
- `providers` for model-specific credentials and routing
- `provider_discovery` (`auto`, `env`, or `config`) to choose whether environment variables and local logins enable providers on their own; see [Restrict providers to config.yaml](/reference/providers-and-models/#restrict-providers-to-configyaml)
- per-command blocks such as `exec`, `ask`, and `edit`
- `commit.message_agent` for the native commit workflow's scope/message agent
- `share` for the built-in GitHub or custom command transcript publisher
- feature-specific blocks such as `image`, `audio`, `music`, `embed`, `search`, `classify`, `sessions`, `file_tracking`, `tools`, and `skills`

### Configuration key index

The index below follows the canonical config schema. A dash means no default or description is specified by that schema; per-surface effective defaults and behavior are explained below. Sensitive values are never printed. Provider fields are described in the provider reference.

<!-- BEGIN GENERATED: configuration -->
| Key | Type | Default | Description |
|---|---|---|---|
| `default_provider` | string | `openrouter` | — |
| `provider_discovery` | string | `auto` | — |
| `auto_compact` | boolean | `true` | — |
| `approval.default_mode` | string | — | — |
| `guardian.backend` | string | `llm` | — |
| `guardian.classify.provider` | string | — | — |
| `guardian.classify.min_confidence` | number | `0.15` | — |
| `guardian.fallback.provider` | string | — | — |
| `guardian.fallback.model` | string | — | — |
| `guardian.fallback.log_path` | string | — | — |
| `guardian.provider` | string | — | — |
| `guardian.model` | string | — | — |
| `guardian.policy_path` | string | — | — |
| `guardian.timeout_seconds` | integer | — | — |
| `guardian.classify_all_shell` | boolean | `false` | — |
| `exec.provider` | string | — | — |
| `exec.model` | string | — | — |
| `exec.approval_mode` | string | — | — |
| `exec.suggestions` | integer | `3` | — |
| `exec.instructions` | string | `""` | — |
| `ask.provider` | string | — | — |
| `ask.model` | string | — | — |
| `ask.approval_mode` | string | — | — |
| `ask.instructions` | string | `You are a helpful assistant.` | — |
| `ask.max_turns` | integer | `50` | — |
| `ask.stdin_inline_max_bytes` | integer | `10240` | — |
| `ask.stdin_max_bytes` | integer | `20971520` | — |
| `chat.provider` | string | — | — |
| `chat.model` | string | — | — |
| `chat.approval_mode` | string | — | — |
| `chat.instructions` | string | `You are a helpful assistant.` | — |
| `chat.max_turns` | integer | `200` | — |
| `chat.terminal_title` | string | `smart` | — |
| `chat.terminal_title_format` | string | `""` | — |
| `chat.terminal_progress` | boolean | `false` | — |
| `commit.message_agent` | string | `commit-message` | — |
| `lifecycle.enabled` | boolean | `true` | — |
| `lifecycle.adapters` | list of string | `["auto"]` | — |
| `lifecycle.osc` | string | `off` | — |
| `lifecycle.commands` | list of map | `[]` | — |
| `share.provider` | string | `github` | — |
| `share.command` | list of string | `[]` | — |
| `share.timeout` | string | `120s` | — |
| `edit.provider` | string | — | — |
| `edit.model` | string | — | — |
| `edit.approval_mode` | string | — | — |
| `edit.instructions` | string | `""` | — |
| `edit.show_line_numbers` | boolean | `true` | — |
| `edit.context_lines` | integer | `3` | — |
| `edit.editor` | string | — | — |
| `edit.diff_format` | string | `auto` | — |
| `image.provider` | string | `gemini` | — |
| `image.output_dir` | string | `~/Pictures/term-llm` | — |
| `image.gemini.api_key` | string | — | — |
| `image.gemini.model` | string | `gemini-2.5-flash-image` | — |
| `image.gemini.image_size` | string | — | — |
| `image.openai.api_key` | string | — | — |
| `image.openai.model` | string | `gpt-image-2` | — |
| `image.chatgpt.model` | string | `gpt-image-2.5-flare` | — |
| `image.xai.api_key` | string | — | — |
| `image.xai.model` | string | `grok-2-image-1212` | — |
| `image.venice.api_key` | string | — | — |
| `image.venice.model` | string | `nano-banana-pro` | — |
| `image.venice.edit_model` | string | — | — |
| `image.venice.resolution` | string | `2K` | — |
| `image.flux.api_key` | string | — | — |
| `image.flux.model` | string | `flux-2-pro` | — |
| `image.openrouter.api_key` | string | — | — |
| `image.openrouter.model` | string | `google/gemini-2.5-flash-image` | — |
| `image.debug.delay` | number | `0` | — |
| `audio.provider` | string | `venice` | — |
| `audio.output_dir` | string | `~/Music/term-llm` | — |
| `audio.venice.api_key` | string | — | — |
| `audio.venice.model` | string | `tts-kokoro` | — |
| `audio.venice.voice` | string | `af_sky` | — |
| `audio.venice.format` | string | `mp3` | — |
| `audio.gemini.api_key` | string | — | — |
| `audio.gemini.model` | string | `gemini-3.1-flash-tts-preview` | — |
| `audio.gemini.voice` | string | `Kore` | — |
| `audio.gemini.format` | string | `wav` | — |
| `audio.elevenlabs.api_key` | string | — | — |
| `audio.elevenlabs.model` | string | `eleven_multilingual_v2` | — |
| `audio.elevenlabs.voice` | string | `JBFqnCBsd6RMkjVDRZzb` | — |
| `audio.elevenlabs.format` | string | `mp3_44100_128` | — |
| `music.provider` | string | `venice` | — |
| `music.output_dir` | string | `~/Music/term-llm` | — |
| `music.venice.api_key` | string | — | — |
| `music.venice.model` | string | `elevenlabs-sound-effects-v2` | — |
| `music.venice.format` | string | `mp3` | — |
| `music.elevenlabs.api_key` | string | — | — |
| `music.elevenlabs.model` | string | `music_v1` | — |
| `music.elevenlabs.format` | string | `mp3_44100_128` | — |
| `transcription.provider` | string | `openai` | — |
| `transcription.model` | string | — | — |
| `transcription.save_dir` | string | `""` | — |
| `transcription.timestamps` | boolean | `false` | — |
| `transcription.venice.api_key` | string | — | — |
| `transcription.venice.model` | string | `nvidia/parakeet-tdt-0.6b-v3` | — |
| `transcription.elevenlabs.api_key` | string | — | — |
| `transcription.elevenlabs.model` | string | `scribe_v2` | — |
| `live.enabled` | boolean | `false` | — |
| `live.provider` | string | `chatgpt` | — |
| `live.instructions` | string | — | — |
| `live.idle_timeout` | string | `10m` | — |
| `live.control_plane` | string | `off` | — |
| `live.classify.provider` | string | — | — |
| `live.classify.log_decisions` | boolean | `true` | — |
| `live.classify.log_state` | boolean | `true` | — |
| `live.classify.min_confidence.status` | number | `0.6` | — |
| `live.classify.min_confidence.new_session` | number | `0.7` | — |
| `live.classify.min_confidence.switch_session` | number | `0.7` | — |
| `live.classify.min_confidence.steer_now` | number | `0.8` | — |
| `live.classify.min_confidence.side` | number | `0.85` | — |
| `live.control_provider` | string | — | — |
| `live.control_model` | string | — | — |
| `live.openai.api_key` | string | — | — |
| `live.openai.model` | string | `gpt-live-1` | — |
| `live.openai.voice` | string | `marin` | — |
| `live.openai.base_url` | string | `https://api.openai.com/v1` | — |
| `live.openai.headers` | map | — | — |
| `live.gemini.api_key` | string | — | — |
| `live.gemini.model` | string | `gemini-3.8-live` | — |
| `live.gemini.voice` | string | `Kore` | — |
| `live.gemini.base_url` | string | `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent` | — |
| `live.chatgpt.model` | string | `gpt-live-1-codex` | — |
| `live.chatgpt.voice` | string | `cove` | — |
| `live.chatgpt.call_base_url` | string | `https://chatgpt.com/backend-api/codex` | — |
| `live.chatgpt.sideband_base_url` | string | `https://api.openai.com/v1` | — |
| `embed.provider` | string | — | — |
| `embed.batch_size` | integer | `32` | — |
| `embed.openai.api_key` | string | — | — |
| `embed.openai.model` | string | `text-embedding-3-small` | — |
| `embed.gemini.api_key` | string | — | — |
| `embed.gemini.model` | string | `gemini-embedding-001` | — |
| `embed.jina.api_key` | string | — | — |
| `embed.jina.model` | string | `jina-embeddings-v3` | — |
| `embed.voyage.api_key` | string | — | — |
| `embed.voyage.model` | string | `voyage-3.5` | — |
| `embed.ollama.base_url` | string | `http://127.0.0.1:11434` | — |
| `embed.ollama.model` | string | `nomic-embed-text` | — |
| `search.provider` | string | `exa_mcp` | — |
| `search.fetch_provider` | string | `jina` | — |
| `search.force_external` | boolean | `false` | — |
| `search.exa.api_key` | string | — | — |
| `search.exa_mcp.url` | string | `https://mcp.exa.ai/mcp` | — |
| `search.exa_mcp.api_key` | string | — | — |
| `search.perplexity.api_key` | string | — | — |
| `search.parallel.api_key` | string | — | — |
| `search.tavily.api_key` | string | — | — |
| `search.brave.api_key` | string | — | — |
| `search.google.api_key` | string | — | — |
| `search.google.cx` | string | — | — |
| `classify.default_provider` | string | `typesafe` | — |
| `classify.providers.typesafe.type` | string | — | — |
| `classify.providers.typesafe.api_key` | string | — | — |
| `classify.providers.typesafe.model` | string | `jev-latest` | — |
| `classify.providers.typesafe.base_url` | string | `https://api.typesafe.ai` | — |
| `classify.providers.typesafe.timeout_seconds` | integer | `10` | — |
| `classify.providers.typesafe.images` | string | `assets` | — |
| `classify.providers.typesafe.headers` | map | — | — |
| `reasoning.display` | string | `auto` | — |
| `reasoning.source` | string | `summary_or_provider_safe` | — |
| `reasoning.status` | string | `title` | — |
| `reasoning.history` | string | `collapsed` | — |
| `reasoning.export` | string | `ask` | — |
| `reasoning.raw` | boolean | `false` | — |
| `reasoning.max_summary_chars` | integer | `12000` | — |
| `reasoning.max_raw_chars` | integer | `20000` | — |
| `reasoning.extract_titles` | boolean | `true` | — |
| `reasoning.hidden_label` | string | `Thinking...` | — |
| `reasoning.persist_summaries` | boolean | `true` | — |
| `reasoning.chat.display` | string | `inherit` | — |
| `reasoning.chat.status` | string | `inherit` | — |
| `reasoning.chat.history` | string | `inherit` | — |
| `reasoning.chat.export` | string | `inherit` | — |
| `reasoning.chat.raw` | boolean | — | — |
| `reasoning.ask.display` | string | `inherit` | — |
| `reasoning.ask.status` | string | `inherit` | — |
| `reasoning.ask.history` | string | `inherit` | — |
| `reasoning.ask.export` | string | `inherit` | — |
| `reasoning.ask.raw` | boolean | — | — |
| `reasoning.serve.display` | string | `inherit` | — |
| `reasoning.serve.status` | string | `inherit` | — |
| `reasoning.serve.history` | string | `inherit` | — |
| `reasoning.serve.export` | string | `inherit` | — |
| `reasoning.serve.raw` | boolean | — | — |
| `reasoning.jobs.display` | string | `inherit` | — |
| `reasoning.jobs.status` | string | `inherit` | — |
| `reasoning.jobs.history` | string | `inherit` | — |
| `reasoning.jobs.export` | string | `inherit` | — |
| `reasoning.jobs.raw` | boolean | — | — |
| `theme.primary` | string | — | — |
| `theme.secondary` | string | — | — |
| `theme.success` | string | — | — |
| `theme.error` | string | — | — |
| `theme.warning` | string | — | — |
| `theme.muted` | string | — | — |
| `theme.text` | string | — | — |
| `theme.spinner` | string | — | — |
| `theme.reasoning_summary` | string | — | — |
| `theme.reasoning_header` | string | — | — |
| `theme.reasoning_raw` | string | — | — |
| `tools.enabled` | list of string | `[]` | — |
| `tools.read_dirs` | list of string | `[]` | — |
| `tools.write_dirs` | list of string | `[]` | — |
| `tools.shell_allow` | list of string | `[]` | — |
| `tools.shell_auto_run` | boolean | `false` | — |
| `tools.shell_auto_run_env` | string | `TERM_LLM_ALLOW_AUTORUN` | — |
| `tools.shell_non_tty_env` | string | `TERM_LLM_ALLOW_NON_TTY` | — |
| `tools.image_provider` | string | — | — |
| `tools.max_tool_output_chars` | integer | `20000` | — |
| `tools.grep_rerank.enabled` | boolean | `true` | — |
| `tools.grep_rerank.provider` | string | — | — |
| `tools.grep_rerank.threshold` | number | `0.5` | — |
| `tools.grep_rerank.timeout_ms` | integer | `2000` | — |
| `tools.grep_rerank.max_candidates` | integer | `150` | — |
| `tool_discovery.mode` | string | `auto` | — |
| `tool_discovery.strategy` | string | `auto` | — |
| `tool_discovery.threshold` | integer | `24` | — |
| `tool_discovery.max_active_tools` | integer | `300` | — |
| `agents.use_builtin` | boolean | `true` | — |
| `agents.search_paths` | list of string | `[]` | — |
| `agents.preferences` | map | — | — |
| `skills.enabled` | boolean | `true` | — |
| `skills.auto_invoke` | boolean | `true` | — |
| `skills.metadata_budget_tokens` | integer | `8000` | — |
| `skills.max_visible_skills` | integer | `50` | — |
| `skills.include_project_skills` | boolean | `true` | — |
| `skills.allow_project_script_tools` | boolean | `false` | — |
| `skills.include_ecosystem_paths` | boolean | `true` | — |
| `skills.always_enabled` | list of string | `[]` | — |
| `skills.never_auto` | list of string | `[]` | — |
| `agents_md.enabled` | boolean | `true` | — |
| `sessions.enabled` | boolean | `true` | — |
| `sessions.max_age_days` | integer | `0` | — |
| `sessions.max_count` | integer | `0` | — |
| `sessions.path` | string | `""` | — |
| `sessions.strip_image_base64` | boolean | `false` | — |
| `diagnostics.enabled` | boolean | `false` | — |
| `diagnostics.dir` | string | `""` | — |
| `debug_logs.enabled` | boolean | `false` | — |
| `debug_logs.dir` | string | `""` | — |
| `loop.approval_mode` | string | — | — |
| `serve.base_path` | string | `/ui` | — |
| `serve.auto_title` | boolean | `true` | — |
| `serve.platforms` | list of string | — | — |
| `serve.approval_mode` | string | — | — |
| `serve.title` | string | — | — |
| `serve.disable_location_sharing` | boolean | `false` | — |
| `serve.files_dir` | string | — | — |
| `serve.widgets_dir` | string | — | — |
| `serve.extensions_dir` | string | — | — |
| `serve.extensions` | string | — | — |
| `serve.response_timeout` | string | `30m` | — |
| `serve.telegram.token` | string | — | — |
| `serve.telegram.allowed_user_ids` | list of integer | — | — |
| `serve.telegram.allowed_usernames` | list of string | — | — |
| `serve.telegram.idle_timeout` | integer | — | — |
| `serve.telegram.interrupt_timeout` | integer | — | — |
| `serve.web_push.vapid_public_key` | string | — | — |
| `serve.web_push.vapid_private_key` | string | — | — |
| `serve.web_push.subject` | string | — | — |
| `serve.mcp.approval_mode` | string | — | — |
| `serve.projects.enabled` | boolean | `true` | — |
| `file_tracking.enabled` | boolean | `true` | — |
| `file_tracking.max_file_bytes` | integer | `2097152` | — |
| `file_tracking.max_session_bytes` | integer | `104857600` | — |
| `file_tracking.max_total_bytes` | integer | `1073741824` | — |
| `file_tracking.max_observation_rows` | integer | `10000` | — |
| `file_tracking.max_observation_session_rows` | integer | `1000` | — |
| `file_tracking.max_observation_bytes` | integer | `16777216` | — |
| `file_tracking.max_observation_session_bytes` | integer | `2097152` | — |
| `file_tracking.max_observation_age_days` | integer | `30` | — |
| `file_tracking.path` | string | `""` | — |
| `linked_devices.broker_url` | string | — | HTTPS rendezvous broker URL; when unset, TERM_LLM_LINKED_DEVICES_BROKER_URL and then the hosted default https://broker.term-llm.com are used. Loopback HTTP is allowed for development. |
| `linked_devices.admission_credential` | string | — | Optional private credential used only to admit host registration at the broker. |
| `linked_devices.diagnostics` | boolean | `false` | Emit a local redacted linked-device transport timeline to stderr. |
| `linked_devices.ice_servers` | list of object | `[{"urls":["stun:stun.term-llm.com:3478"]}]` | Ordered ICE policy shared by the host and approved devices; free STUN by default, TURN requires explicit configuration, and [] uses host candidates only. |
<!-- END GENERATED: configuration -->

### Classification

The [`classify` command](/guides/classify/) uses `classify.providers`, separate from chat `providers`. Three providers are built in and work from environment variables alone (Cloudflare also needs `CLOUDFLARE_ACCOUNT_ID`):

| Provider | `type` | Default `model` | Default `base_url` | API key fallback |
| --- | --- | --- | --- | --- |
| `typesafe` (default) | `typesafe` | `jev-latest` | `https://api.typesafe.ai` | `TYPESAFE_API_KEY` |
| `openai` | `openai` | `gpt-6-luna` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| `cloudflare` | `cloudflare` | `clef` | `https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run` | `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_AUTH_TOKEN` |

```yaml
classify:
  default_provider: typesafe
  providers:
    typesafe:
      type: typesafe # Optional for built-in providers
      api_key: ${TYPESAFE_API_KEY}
      model: jev-latest
      base_url: https://api.typesafe.ai
      timeout_seconds: 10
```

- **Aliases:** other names under `classify.providers` must declare one of the types and inherit its defaults. They expand into their own keys for `config show`, `config get`, and completion, and their `api_key` is redacted like the built-in ones.
- **Credentials:** `api_key` is sensitive and resolved only for the selected provider, when a command runs or when a classify-backed Guardian starts. The environment fallback is used only with the type's default endpoint; a custom `base_url` or a `--base-url` override sends only the configured `api_key`.
- **Cloudflare account:** `{account_id}` is filled from `CLOUDFLARE_ACCOUNT_ID`; or write the account ID into `base_url`.
- **Images:** every provider accepts `classify --image`. For `typesafe`-type providers, `images` selects the wire format: `assets` (default) for TypeSafe's experimental assets map, with the `TypeSafe-Danger-Zone-Images` header added to image requests only, or `inline` for an `images` array of data URLs on compatible self-hosted servers. Other values, and `images` on other types, are rejected.
- **Overrides:** `--provider/-p` replaces `classify.default_provider`; `--model`, `--base-url`, and `--timeout` override one run.

Other commands call a classification provider only when `guardian.backend: classify` or `live.control_plane: classify` is set, or when the `grep` tool is called with its `intent` parameter and [intent reranking](/reference/built-in-tools/#intent-based-grep-reranking) is available. State and questions are sent to the configured endpoint; see the [privacy guidance](/guides/classify/#privacy).

Grep intent reranking is controlled by `tools.grep_rerank` and is on by default, but it only takes effect when the selected classify provider has usable credentials:

```yaml
tools:
  grep_rerank:
    enabled: true        # false removes the grep `intent` parameter entirely
    provider: ""         # optional classify provider; empty uses classify.default_provider
    threshold: 0.5       # keep results scoring at or above this (0–1)
    timeout_ms: 2000     # hard wall-clock budget for all added work (1–10000)
    max_candidates: 150  # candidates searched and scored per call (2–500)
```

An unknown `provider` name does not fail config loading; it leaves reranking unavailable. Out-of-range numeric values are rejected.

## Example

```yaml
default_provider: openrouter

providers:
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
    # Uses OPENAI_API_KEY (or an explicit api_key).
    # HTTP/SSE is the default; set true to opt into WebSockets.
    use_websocket: false

  chatgpt:
    # Separate subscription OAuth: term-llm auth login chatgpt
    model: gpt-5.6-sol-medium
    use_websocket: true

  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
    model: qwen2.5-coder:7b

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
    model: openrouter/free
    fast_model: openrouter/free
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm
    # Optional privileged credential, resolved only for account usage analytics:
    management_key: ${OPENROUTER_MANAGEMENT_KEY}

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

commit:
  # Defaults to commit-message and uses normal agent registry precedence.
  message_agent: commit-message

lifecycle:
  enabled: true
  adapters: [auto] # environment-detected Herdr/cmux only
  osc: off         # no terminal progress without explicit opt-in
  commands: []     # no custom process without explicit configuration

share:
  provider: github # or command
  command: []      # direct argv; required only for provider: command
  timeout: 120s    # create/update; maximum 600s

# Optional global approval override. When omitted, chat, ask, and ordinary
# serve platforms use auto; edit, exec, loop, and serve mcp use prompt.
# approval:
#   default_mode: prompt

guardian:
  # Optional: override the provider/model used for auto approval review.
  # Without overrides, Guardian uses the fast model on default_provider.
  # It never inherits per-command, per-surface, session, or agent overrides.
  # provider: anthropic
  # model: claude-sonnet-4-6
  # policy_path: ~/.config/term-llm/guardian-policy.md
  # Review timeout in seconds. Defaults to 90.
  # timeout_seconds: 90
  # Suspend every shell pattern while auto mode is active.
  classify_all_shell: false

edit:
  provider: openai
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

A blank configuration uses these built-in defaults:

| Surface | Default |
|---------|---------|
| `chat`, `ask`, ordinary `serve` platforms | `auto` |
| `edit`, `exec`, `loop`, `serve mcp` | `prompt` |

Ordinary `serve` means the `web`, `api`, `jobs`, and `telegram` platforms, including combined launches. To start one in human approval mode instead, pass `--approval prompt` or set `serve.approval_mode: prompt`. Standalone `term-llm serve mcp` remains prompt by default.

In **prompt** mode, actions outside deterministic file/directory and shell allowlists ask before proceeding. In **auto** mode, deterministic exact approvals and usable narrow patterns still run first. Guardian reviews unmatched shell commands, file reads, file writes and edits/diffs, grep/glob directory searches, image input/output paths, and explicit additional-workspace grants/elevations. Auto does not review MCP/network/service actions and is not yolo. Shell approvals from Guardian are cached only for the exact command and working directory; ordinary per-file reviews do not silently create workspace grants.

Local launches record the canonical runtime/project directory as a per-session **proposed primary workspace**, initially with zero file authority. `term-llm chat` asks for direct human whole-workspace read/write confirmation at startup whenever a registered local file, search, or image tool could need that authority; other interactive surfaces ask on first path access inside the proposal. This decision happens before Guardian or deterministic file rules. The prompt shows the workspace root, defaults to **Always allow** (remember it for future sessions), and also offers **Allow this session** or **Deny**; shell remains separately controlled. Esc/Ctrl+C on the proactive startup prompt defers the decision until a path tool is used, while explicit **Deny** remains latched for the session. Session-only allow is persisted with the root session so resume does not reprompt. Remembered allow stores the exact canonical root in `$XDG_CONFIG_HOME/term-llm/remembered-workspaces.yaml` (falling back to `~/.config`) and applies to later sessions with that same root. Deny blocks workspace path access, and no confirmation transport fails closed unless the workspace was previously remembered. While yolo is effective, file access bypasses this confirmation without prompting or persisting workspace authority; after leaving yolo, the next access prompts if the primary remains unconfirmed and unremembered. Worktree rebinding invalidates a mismatched primary confirmation while preserving additional grants.

Serve/web never derives even a proposal from daemon CWD. Only explicit request/session state can propose a workspace, and web uses the same dedicated first-access confirmation modal outside yolo. Before a session/request/worktree is explicitly bound, relative local-tool paths and shell calls without an absolute `working_dir` fail closed rather than using daemon CWD; absolute targets still pass through normal approval checks. Guardian and headless/synchronous operation cannot substitute for the human workspace decision; explicit yolo bypasses it for tool access without establishing primary authority.

Whenever any local file/search/image path tool is enabled—including through an explicit tool list—term-llm also exposes `manage_workspace`. Additional workspaces default to read-only, are canonicalized and narrowed to their enclosing Git worktree root when applicable, and can be explicitly elevated to write. Auto mode Guardian-reviews each new grant and each read-to-write elevation once; duplicate/weaker grants are idempotent. Grants can be listed or revoked, are shared immediately with child agents, never grant shell/MCP/network authority, and never modify project/global approvals. SQLite sessions persist non-yolo grants across resume and copy them to conversation branches; stores without the optional persistence capability keep runtime-only grants. Yolo grants are always ephemeral overlays: they are never persisted or branched, disappear throughout the parent/child tree on leaving yolo, and a temporary write elevation restores any durable read grant beneath it.

Choose a mode for one invocation with `--approval`:

```bash
term-llm chat --approval prompt   # conservative override of chat's auto default
term-llm serve web --approval prompt # conservative override of serve's auto default
term-llm ask --approval auto "fix this"
term-llm loop --approval yolo --done "go test ./..." "fix tests"
```

`--auto` and `--yolo` remain compatibility aliases for `--approval auto` and `--approval yolo`. The three flags are mutually exclusive. Yolo cannot be set in persistent configuration and is never restored on a cold resume.

Set one persistent global default for all surfaces:

```yaml
approval:
  default_mode: prompt # prompt or auto
```

Or override individual surfaces:

```yaml
approval:
  default_mode: auto

edit:
  approval_mode: prompt

serve:
  approval_mode: prompt # override ordinary serve's auto default
  projects:
    enabled: true
  mcp:
    approval_mode: prompt
```

Configuration accepts only `prompt` and `auto`. An omitted or empty surface value inherits an explicitly configured `approval.default_mode`; if neither is set, the built-in matrix applies. `term-llm config show` lists each effective surface mode and its resolution source, and `term-llm config get chat.approval_mode` reports the effective value when the key is unset. Resolution precedence is explicit CLI mode, persisted session mode (for chat resume), per-surface config, global config, then the built-in default. A legacy resumed chat with no stored mode resumes in prompt, and stored yolo is downgraded to prompt.

### Web project registry

`term-llm serve web` enables the project registry by default. A project is a shared, durable pointer to an existing directory on the server. Configure it with:

```yaml
serve:
  projects:
    enabled: true
```

Use `--no-projects` for single-workspace mode. This is the intended setup for dedicated personal agents and agent containers whose persistent workspace is the whole environment, rather than one project among many. It keeps the flat/date sidebar and binds first-party default-workspace requests to the serve startup directory's main Git root; a non-Git startup remains unbound. Use `--projects` for strict project-registry opt-in: startup fails if project storage is read-only/unsupported or the only bootstrap candidate is filesystem root. Default-enabled project startup is rollout-tolerant and instead warns and falls back to single-workspace mode for those cases. Project initialization runs only when the `web` platform is selected; API-, jobs-, Telegram-, and Hub-delegated work do not acquire an implicit project.

Project paths are absolute paths on the machine running term-llm. Symlinks are resolved. A directory inside a Git checkout is normalized to the main repository root; a non-Git directory remains exact. Filesystem root cannot be registered—use `--no-projects` single-workspace mode with the existing read/write directories, grants, shell policy, and approval mode for container-wide agents.

A registered project is metadata, not authorization. Selecting one does not bypass workspace confirmation, static read/write allowlists, shell approval, or Guardian.

Auto mode is intentionally narrower than yolo:

- `prompt`: ask before unapproved tool actions; existing pattern rules retain their prior behavior.
- `auto`: Guardian reviews supported unmatched local operations and fails closed on denials/review failures.
- `yolo`: auto-approve tool actions without prompting (explicit runtime selection only).

Interactive Guardian initialization failure produces one warning and temporarily uses prompt mode; the requested auto policy remains saved so a later resume can retry. Headless auto runtimes fail startup if Guardian cannot initialize. Because ordinary `serve` now uses the auto built-in default, an unconfigured serve launch follows this fail-fast path; use `--approval prompt` or `serve.approval_mode: prompt` to opt into human approval mode instead. Once auto is running, a Guardian denial, contradictory allow, timeout, malformed response, unavailability, or transport failure returns an error to the agent in terminal, web/serve, and headless runtimes. It does not immediately fall through to a human prompt for that action.

Configure Guardian review with:

```yaml
guardian:
  backend: llm             # default; or classify (see below)
  provider: anthropic       # optional override
  model: claude-sonnet-4-6  # optional authoritative model pin
  policy_path: ~/.config/term-llm/guardian-policy.md # optional custom policy
  timeout_seconds: 90       # optional; default 90
  classify_all_shell: false # true sends every shell pattern through Guardian
```

For the default `llm` backend, without explicit Guardian overrides, resolution uses the configured `fast_model` for the global `default_provider`, switching to its `fast_provider` when configured. Per-command, per-surface, session, and agent provider/model overrides do not change that target. `guardian.provider` explicitly selects another provider and its own `fast_model` without following that provider's `fast_provider` (a `fast_model` paired with a different `fast_provider` is ignored); `guardian.model` pins the model on `guardian.provider` or, when omitted, on the global default provider. For compatibility, a custom or local provider with no `fast_model` uses its configured `model` before considering a built-in fast default, which only applies on the provider's stock endpoint.

In auto mode, arbitrary-execution shell patterns are suspended before matching. The mechanical set includes executable globs (`*`, `*/bin/*`) plus wildcard arguments to interpreters, shells, elevation tools, and common dispatchers. Examples include `python *`, `python *.py`, `/usr/bin/python3 *`, `node *.js`, `bash *`, `env *`, `sudo *`, `uv run *`, `npx *`, and `pipx run *`. Existing generated rules of this form continue to work after an explicit switch to prompt mode but are intentionally ignored while auto is requested. Narrow fixed commands such as `git status`, `go test *`, `npm test`, `python script.py`, `uv run pytest`, `npx eslint`, and `pipx run black` remain deterministic unless `classify_all_shell` is true. Exact configured scripts, exact session commands, and exact Guardian command/workdir approvals remain active either way.

Configured, session, ancestor, and project pattern sets remain independent; term-llm does not union them to cover different segments of a compound command. Safe pipe targets can satisfy only a later pipe segment after the head has a usable deterministic approval, so they cannot restore a suspended head. A suspended pattern is not a deny: the unresolved exact action goes to Guardian.

Three consecutive Guardian policy denials or 20 total policy denials in the current auto epoch suspend auto across the root manager and all child agents. Successful Guardian approval resets only the consecutive count; reviewer failures do not count. The threshold-triggering action remains denied and the next approval-bearing action uses effective prompt mode. The requested auto policy remains responsible for shell-pattern filtering while suspended, so arbitrary-execution rules—or all rules under `classify_all_shell`—cannot become implicit post-breaker approvals. In the TUI, the first Shift+Tab after suspension calls the explicit auto-resume path, clears the latch/counters, and starts a fresh epoch; it does not jump directly to yolo. An explicit switch out of and back into auto also starts a fresh epoch. Suspension affects runtime behavior only, not the requested/persisted policy, so a cold resume may enter auto with a fresh epoch. In Web, `/approvals` offers Prompt, Auto, and Yolo for the current conversation and **Resume Auto** after suspension. These overrides are runtime-only. Controls require approval-managed tools and are unavailable on yolo-launched servers; child conversations inherit their parent’s policy and cannot override it independently. Auto requires an available Guardian.

To override a denial deliberately, explicitly authorize the exact action in a subsequent message so Guardian can reassess it using the new trusted transcript evidence, or switch the session to prompt/yolo using existing controls. Guardian never directly offers a wildcard command pattern, directory, or repository grant after a denial.

The optional classify backend uses a separately configured classification provider:

```yaml
guardian:
  backend: classify              # llm remains the default
  classify:
    provider: typesafe            # optional; inherits classify.default_provider
    min_confidence: 0.15          # inclusive range [0, 1]; distribution spread, not certainty
  fallback:                       # optional LLM reviewer for classifier denials/failures
    provider: chatgpt             # optional; same rules as guardian.provider
    model: gpt-5.6-luna-low       # optional; same rules as guardian.model
    log_path: ~/.local/share/term-llm/guardian/escalations.jsonl # optional; `off` disables
```

Unknown backend values fail Guardian installation. The selected classify provider, endpoint and API key are resolved eagerly at setup; no review call is made until an action requires it. `guardian.provider`/`model` are LLM-only; classify uses the selected `classify.providers` model and transport timeout. Both backends retain `policy_path`, Guardian's review timeout, yolo bypass, callbacks, breaker and interactive/headless behavior. The earlier of the Guardian and classify transport deadlines wins.

Classification submits three choice questions together. Allow requires outcome `allow`, risk `low`/`medium`, authorization evidence `explicit`/`implied` (mapped to Guardian's existing `high`/`medium` values), and every confidence present and at least `min_confidence`. Malformed answers, unknown choices and missing confidence are review errors; without a fallback, low confidence is a deterministic policy denial and counts toward the breaker. Only actual `user`/`parent_user` roles supply trusted authorization. Successful first-party `ask_user` answers are explicitly marked by the runtime as trusted user input, carrying only the displayed question and the user's own selection; model-authored option descriptions are never quoted as user speech, and no other tool can mint a trusted user turn. See [Classification](/guides/classify/#guardian-backend) for details. Both backends expose callback latency as `duration_ms` in Guardian JSON events; classify also supplies `state_bytes` for serialized state. Classification model costs (TypeSafe, OpenAI Decisions, or Clef) remain unpriced, never borrowed from the chat model.

> Privacy note: Guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If `guardian.provider` or the selected `fast_provider` differs from the chat provider, that evidence is sent to the resolved Guardian provider as well. With `guardian.backend: classify`, the policy, compact role-labelled transcript, omitted count, approval context and exact shell/file/directory/workspace action are sent to the selected classification provider's endpoint (TypeSafe, OpenAI, or Cloudflare), even without running the `classify` command. With `guardian.fallback`, an escalated action sends the same evidence to the resolved fallback provider too, and the classifier request and both verdicts are written to the escalation log on disk. Compaction is not redaction; only enable this if sending that evidence is authorized.

### Classify with an LLM fallback

With `guardian.fallback`, the classifier runs first and its **allow** is final: the fast path sends no LLM request, writes no log record, and the approval manager's existing contradiction check still applies. A classifier **deny** (including a low-confidence gate) or a classifier **failure** (transport error, malformed or incomplete answers, over-budget request) escalates the same action to a standard LLM Guardian reviewer, and that LLM verdict becomes the decision. The fallback reviewer is resolved and constructed eagerly at setup, exactly like the LLM backend, so a missing provider or credential fails installation instead of surfacing on the first escalated action; the failure behavior is the same as any other Guardian setup failure (one warning plus prompt mode interactively, startup failure headless).

On escalation the approval manager applies its normal checks to the LLM verdict: a fallback allow whose risk is high/critical or whose user authorization is low/unknown is still denied as contradictory and counts toward the breaker. A classify denial counts toward the breaker only when the fallback cannot review the action: if the classifier produced a well-formed denial and the fallback review then fails, the classifier's denial stands and is enforced as a policy denial, with the fallback failure noted in the rationale (`fallback unavailable: ...`). Only a classifier failure combined with a fallback failure is a review failure, which fails closed without counting, exactly like an LLM-backend review failure. The approval event's status line is marked `via fallback` (for example `approved (low risk; clearly user-authorized; via fallback)`), and escalated denial messages carry the rationale, which is prefixed with `escalated from classify (...)`.

`guardian.fallback.provider` and `guardian.fallback.model` use the same resolution rules as `guardian.provider` and `guardian.model`, and at least one of them must be set for the fallback to exist: with neither set the fallback is disabled and the classify backend runs alone. `provider` alone selects that provider's fast model; `model` alone pins the model on the resolved default provider; setting both pins the model on that provider. Each reviewer applies `guardian.timeout_seconds` to its own call, so an escalated review can take up to roughly twice that timeout (classifier first, then the LLM reviewer); total latency also includes reviewer-pool queueing, so it is not a hard bound. `guardian.fallback` is rejected on the `llm` backend (or with no explicit backend) and a `log_path` alone is rejected without a provider or model.

**Escalation log:** every escalation appends one JSON line to `$XDG_DATA_HOME/term-llm/guardian/escalations.jsonl` (falling back to `~/.local/share/term-llm/guardian/escalations.jsonl`); set `guardian.fallback.log_path` to use another file, a `~`-prefixed path, or the literal `off` to disable logging. A custom path must be absolute or start with `~`: a relative path is rejected when the configuration loads, and a `~` path that still expands to a relative location disables logging with one warning. There is deliberately no working-directory fallback: when no home directory can be resolved, logging is disabled with one warning and escalation still works. The file is created with mode `0600` in a `0700` directory, and an existing path that is not a regular file (for example a symlink) is refused so evidence is never written somewhere unintended. Each record contains the schema version and timestamp, the scope ID, the exact classifier request that was constructed (state, model and questions) along with whether it was sent, `min_confidence`, the classifier stage (`build`, `transport`, `validate`, or `decision`), the answers when they arrived, the classifier verdict or error, the reported `classify_model` and `classify_usage`, the classifier duration, and the fallback provider, model, raw verdict or error, `fallback_usage` and duration. The classifier-side data exists to fine-tune the classifier later. The fallback verdict is the raw pre-enforcement reviewer output: the approval manager may still deny it, so read it together with the recorded event rather than as the final decision. The log grows without bound; rotate or delete it yourself. Logging failures never change a review result and warn at most once per Guardian installation.

> Fallback privacy: an escalated review sends the same evidence as an LLM Guardian review to the fallback provider (the policy, compact transcript and approval context, plus the exact action), on top of the classification request described above, and the classifier request and both verdicts are written to the escalation log. Only enable the fallback when sending that evidence to both destinations is authorized.

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
  openrouter:
    model: openrouter/free

exec:
  provider: openrouter
  model: openrouter/free

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

Agentic commands can make multiple provider calls while they execute tools and feed results back to the model. `max_turns` caps that loop. If turn N (the final permitted provider turn) requests tools, those tools **still execute**, and their results are persisted before a turn-limit error is returned. The model does not receive another turn to interpret those results until the session is resumed. This applies to all runs, not just delegated agents: N turns now allow N tool rounds instead of N-1. Check tool side effects before resuming or retrying a limited run.

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

## Ask stdin limits

`term-llm ask` embeds small text from stdin or `-f` directly and stages larger text, binary files, and supported images under the private app uploads directory. Configure the per-source byte thresholds under `ask`:

```yaml
ask:
  stdin_inline_max_bytes: 10240   # 10 KiB; must be positive
  stdin_max_bytes: 20971520       # 20 MiB hard ceiling; must be >= inline limit
```

Override the inline threshold for a single run with `term-llm ask --inline-max-bytes <size>` (for example `512K`, `2MiB`, or a plain byte count). The override must be positive and may not exceed `ask.stdin_max_bytes`. Use it for scripted runs that deliberately pass a large prompt file and cannot spend a turn on `read_file`.

The maximum applies independently to each resolved `-f` item and to stdin. It is enforced while reading (`max + 1` bytes), so oversized streams and regular files are rejected without being fully buffered or staged.

## Parallel tool execution

Models may request many independent tool calls in a single turn, such as several `read_file`, `grep`, or `glob` calls. term-llm executes independent tool calls concurrently when parallel tool calls are enabled by the provider/request, but caps one model turn at **20 concurrently running tool calls**. Additional tool calls from the same turn are queued and run as earlier calls finish.

This is a built-in safety limit rather than a config option today. It preserves useful batching while preventing a single response from spawning an unbounded number of shells, greps, reads, or subagents at once.

## Native commit agent

`commit.message_agent` defaults to `commit-message`. The value must be a safe exact agent registry lookup name. Normal precedence applies, so a project-local or user-global `commit-message` shadows the builtin; setting another name selects that agent explicitly. Resolution errors are shown in `/commit` and leave manual message entry available—there is no silent builtin fallback. The workflow keeps the agent's style/model preferences but overlays read-only, host-owned scope and staged-message tools and always suppresses `on_complete`.

## Chat terminal titles

`chat.terminal_title` controls whether interactive chat updates terminal titles:

- `smart` (default): update the terminal/tab title and, inside tmux, rename the current tmux window.
- `basic`: emit only the terminal/tab title escape; tmux window names are not managed.
- `off`: emit no title escape sequences and do not rename tmux windows. Lifecycle OSC is controlled separately by `lifecycle.osc`.

```yaml
chat:
  terminal_title: smart
```

`chat.terminal_title_format` optionally customizes the text used for both the terminal/tab title and smart tmux window title. Leave it empty for the built-in smart format (`task · branch · agent · model`, with the `branch` segment omitted for root sessions and with streaming/attention variants).

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
- `{{branch}}` - `branch` when the current session is a child conversation path, otherwise empty.
- `{{phase}}` - current phase, such as `Thinking` or `Responding`.
- `{{state}}` - `idle`, `streaming`, or `attention`.
- `{{activity}}` - model name while idle, streaming activity while streaming, or `attention` while waiting for input.
- `{{elapsed}}` - streaming elapsed time such as `12s` when available.
- `{{attention}}` / `{{attention_marker}}` - `‼` when approval/ask-user/handover attention is needed, otherwise empty.
- `{{env "NAME"}}` or `{{env.NAME}}` - environment variable from the chat process. Use this sparingly: titles may appear in the terminal, window manager, tmux, screen shares, and logs. Do not include secrets.

The format is a Go `text/template`, so pipelines work too. A small `default` helper returns its first argument when the piped value is empty:

```yaml
chat:
  terminal_title_format: "[{{env \"DOCKER_CONTAINER_NAME\" | default \"host\"}}] {{title}}"
```

Title text is sanitized before emission: control characters are removed, whitespace is collapsed, and very long results are truncated.

Ghostty honors `OSC 2` title updates but may override or ignore program-supplied titles. Terminal lifecycle progress is separate and off by default; explicitly set `lifecycle.osc: auto` to emit Ghostty's `OSC 9;4` indicator for working/blocked state, or `on` to force the protocol. Inside tmux, term-llm uses passthrough so Ghostty can receive the sequence; tmux 3.3+ may require `set -g allow-passthrough on` in `~/.tmux.conf`.

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

## Transcript sharing

`share.provider` defaults to `github`, which publishes through the authenticated `gh` CLI. Set it to `command` to use a Share Helper Protocol v1 executable:

```yaml
share:
  provider: command
  command: [/usr/local/bin/acme-share, --tenant, engineering]
  timeout: 120s
```

`share.command` is an argv list, not shell text, and may include prefix arguments. The operation name is appended by term-llm. The timeout must be greater than zero and no more than `600s`; capability discovery is separately capped at ten seconds. A command is rejected under the GitHub provider so an ignored helper configuration cannot look active.

See [Sharing](/reference/sharing/) for TUI/CLI behavior and the complete helper JSON protocol.

## Terminal-host lifecycle

`lifecycle` controls publication of the visible chat only when the command's
invoking stdin and stdout are interactive. Tool-spawned/captured `--auto-send`
children do not acquire terminal-host authority; direct terminal `--auto-send`
launches do:

```yaml
lifecycle:
  enabled: true
  adapters: [auto] # or [herdr], [cmux], [herdr, cmux], []
  osc: off         # off, auto, on
  commands:
    - name: tmux
      command: [/home/you/.local/bin/term-llm-tmux-lifecycle]
      timeout: 2s
```

`auto` selects only environment-detected first-party adapters and cannot be mixed with explicit adapter names. A command entry is itself an explicit opt-in: term-llm executes the listed executable directly (no shell), passes the remaining entries as literal argv, writes one versioned JSON event to stdin, and discards output. Empty executables, duplicate/reserved names, invalid adapter/OSC modes, NUL arguments, oversized argv, and timeouts outside `(0, 30s]` are rejected during config load.

The global `TERM_LLM_LIFECYCLE=0` opt-out and per-adapter `TERM_LLM_HERDR=0` / `TERM_LLM_CMUX=0` opt-outs override config and skip disabled discovery. `chat.terminal_progress: true` is retained as a deprecated explicit alias for `lifecycle.osc: auto` in its historical smart-title/Ghostty-detection scope; new configurations should use `lifecycle.osc`. Set `TERM_LLM_LIFECYCLE_DEBUG=1` for rate-limited adapter/error-class diagnostics that omit argv, environment, raw errors, cwd, and session data.

Use `term-llm lifecycle status [--json]` for read-only discovery details. See [Terminal-host lifecycle](/guides/terminal-host-lifecycle/) for state semantics, the JSON schema, security/privacy guidance, shutdown/reload behavior, and complete tmux/Zellij bridges.

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
  enabled: true
  max_file_bytes: 2097152 # 2 MiB per-file content cap
  max_session_bytes: 104857600 # 100 MiB retained content per session
  max_total_bytes: 1073741824 # 1 GiB attributed-history data cap across sessions
  max_observation_rows: 10000
  max_observation_session_rows: 1000
  max_observation_bytes: 16777216 # 16 MiB independent metadata cap
  max_observation_session_bytes: 2097152 # 2 MiB per session
  max_observation_age_days: 30
  path: "" # optional attributed DB path override; observation sidecar shares its directory
```

Enabled by default. term-llm records retained before/after content only for attributed transitions: trusted direct mutation tools, or shell `transform`/`generate` claims declared before execution and subsequently verified. Materializations and unclaimed effects are metadata-only observations and never contribute Agent Changes totals.

To disable file tracking, set:

```bash
term-llm config set file_tracking.enabled false
```

or add the following override to your config file:

```yaml
file_tracking:
  enabled: false
```

The existing `term-llm serve web --enable-file-tracking` flag can still force tracking on for one server invocation when the saved config disables it.

**Privacy note:** attributed content is persisted to `~/.local/share/term-llm/file_history.db`, separate from `sessions.db`. Retained blobs are gzip-compressed and content-addressed. Oversized, unsupported binary, unknown-side, and over-budget transitions keep provenance metadata but not a fabricated diff. `max_total_bytes` applies to growing attributed data with a small fixed structural reserve for mandatory indexes. History for deleted sessions is swept on startup, following `sessions.max_age_days`.

Observations and materializations contain bounded counts, roots, sampled paths, coverage, and closed diagnostic details only—never blobs, command text, environment values, headers, credentials, URL userinfo, or unsanitized URLs. They live in the physically separate `file_observations.db` sidecar and have independent row, byte, per-session, and age limits. Observation pressure therefore cannot evict attributed blobs.

`affected_paths` bounds shell inspection only and does not attribute transitions. Use pre-execution `output_claims` for task transforms/generators and `materialize` claims for imported content. Without claims, bounded Git/session-path discovery may produce observations, but never agent changes. Incomplete coverage and tracker failures are surfaced as diagnostics.

## MCP tool discovery config

```yaml
tool_discovery:
  mode: auto
  strategy: auto
  threshold: 24
  max_active_tools: 300
```

| Key | Default | Meaning |
|---|---:|---|
| `mode` | `auto` | `auto` eagerly exposes catalogues at or below `threshold` and defers larger catalogues; `eager` and `deferred` force either behavior. |
| `strategy` | `auto` | `auto` uses verified provider-native discovery when available and otherwise portable `tool_search`; `portable` or `native` force one path. |
| `threshold` | `24` | Authorized MCP tool count at or below which `auto` mode remains eager. |
| `max_active_tools` | `300` | Maximum unpinned, dynamically visible MCP schemas. A value of `0` selects the default 300. |

When the dynamic working set is full, term-llm first evicts tools that have not yet been sent to the provider, then never-called tools, and finally least-recently-used called tools. Pinned and MCP `always_load` tools are outside this limit and are never evicted. Eviction affects visibility and context usage, not authorization; an evicted tool remains discoverable and can be activated again. If all eligible victims were already sent, eviction may reset provider conversation or prompt-cache state so the changed schema surface is applied safely.

Tool-discovery diagnostics report `dynamic_active`, `dynamic_limit`, and `eviction_count`; the same count is exposed by the serve MCP session JSON response. See the [MCP servers guide](/guides/mcp-servers/#deferred-tool-discovery) for server configuration and discovery details.

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

  parallel:
    api_key: ${PARALLEL_API_KEY}

  tavily:
    api_key: ${TAVILY_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}

  google:
    api_key: ${GOOGLE_SEARCH_API_KEY}
    cx: ${GOOGLE_SEARCH_CX}
```

Defaults are `provider: exa_mcp` and `fetch_provider: jina`: external search uses Exa's remote MCP server, while `read_url` uses Jina Reader. Set `fetch_provider: exa_mcp` to fetch pages through Exa MCP as well, or `fetch_provider: none` to omit the external `read_url` tool.

See the [Search guide](/guides/search/) for detailed search configuration.

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

Each feature block can hold provider-specific credentials and defaults. The image, audio, music, transcription, and embedding providers are independent of the main text provider. `image.provider` also accepts a named `providers` entry with `type: openai_compatible`, an API `base_url`, and a `model`; see [local image providers](/guides/image-generation/#local-and-openai-compatible-image-providers).

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
- `file://path#nested.field` → field extracted from a JSON or YAML (`.yml` / `.yaml`) file
- `op://...` → 1Password secret lookup
- `$()` → command output
- `${VAR}` / `$VAR` → environment variable expansion

This is passed only to the provider subprocess. It does not mutate your parent shell environment.

## Responses API continuation

Providers using `type: openai` (including custom Responses API endpoints) default
to `use_previous_response_id: true`. Compatible follow-up requests reference the
previous response and send only new input, avoiding full-history uploads.

If an endpoint cannot reliably retain or route prior responses, disable this
behavior for that provider:

```yaml
providers:
  muse_wonder:
    type: openai
    api_key: ${MUSE_API_KEY}
    base_url: https://your-provider.example/v1
    model: muse-wonder-1.0-contributor
    use_previous_response_id: false
    request_encrypted_reasoning: true
```

Setting `use_previous_response_id: false` omits `previous_response_id` and sends
full local history on every request, including tool-result follow-ups. It applies
to both HTTP/SSE and WebSocket transport; `use_websocket` remains independent.
Keep `request_encrypted_reasoning` enabled (the default) to preserve reasoning
state for replay. If you previously disabled it, explicitly set it back to `true`;
this cannot recover reasoning missing from earlier responses.

Full-history requests upload more data; any prompt-cache savings depend on the
provider. This option does not change the `store` field or disable prompt caching.
It applies only to `type: openai`, not `openai_compatible` (Chat Completions),
`chatgpt`, or `opencode-go`. Omitting the option preserves existing behavior.

## Encrypted reasoning requests

Providers using `type: openai` (including custom Responses API endpoints) request
`reasoning.encrypted_content` by default. This opaque reasoning data is retained
for full-history replay if server-side conversation state becomes unavailable.

Some endpoints, such as Muse Wonder, reject this include option together with
`previous_response_id`. Either disable [response-ID continuation](#responses-api-continuation)
and retain encrypted reasoning for full-history replay, or disable the include:

```yaml
providers:
  muse_wonder:
    type: openai
    api_key: ${MUSE_API_KEY}
    base_url: https://your-provider.example/v1
    model: muse-wonder-full-1
    request_encrypted_reasoning: false
```

`request_encrypted_reasoning` defaults to `true`. Setting it to `false` omits
`reasoning.encrypted_content` from the include list on all requests, including
full-history retries, without disabling efficient `previous_response_id`
continuation. It does not delete or prevent replay of encrypted reasoning already
saved in history or returned unsolicited by the provider.

**Recovery trade-off:** messages and tool calls are still retained, but full-history
recovery may lack reasoning state for responses made with this option disabled.
This setting applies only to `type: openai`, not `openai_compatible` (Chat
Completions), `chatgpt`, or `opencode-go`.

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
- `reasoning_mode: pro` is public-API Pro mode and is not sent through ChatGPT OAuth. Codex's product-level Ultra option is also not a reasoning effort; it combines `max` effort with subagents.
- `reasoning_context` accepts only `auto`, `current_turn`, or `all_turns`.
- Enabling multi-agent without a concurrency value uses `3`. Multi-agent requests omit the normal reasoning-summary request, send the required beta header, and use HTTP/SSE even when provider WebSocket transport is enabled; WebSocket `response.inject` support is not yet implemented.
- Programmatic tool calling requires at least one listed tool, and every name must also be present in that request's tool definitions.
- Prompt-cache `mode` accepts `implicit` or `explicit`; the only supported explicit TTL is `30m`.
- Request-level options override populated provider defaults. Ephemeral internal requests, such as title and helper calls, do not inherit provider advanced defaults unless the caller explicitly supplies options.

In terminal chat, `/pro on`, `/pro off`, and `/pro status` set the session's reasoning mode. The setting is persisted with the session and automatically disabled if you switch to a provider/model that does not support it.

## Provider file upload policy

Provider configs can override which MIME types may be forwarded as native file/document inputs. This matters for the web/API `input_file` path: term-llm saves uploads locally first, then either sends a native file part, embeds text-like files as prompt text, or falls back to a local marker for unsupported binaries.

Built-in defaults are conservative:

- `openai`, `chatgpt`, `grok`, and `copilot` accept OpenAI's exact file-input token list by default: PDF; Word (`.doc`/`.docx`), RTF, OpenDocument text, Pages, and Google Docs; Excel (`.xls`/`.xlsx`), CSV, TSV, IIF, and Google Sheets; PowerPoint (`.ppt`/`.pptx`), Keynote, and Google Slides; and the documented text and code tokens (`text/x-ruby`, `text/x-python`, `application/json`, and the like). The list has no wildcards, so a client label is canonicalized onto an accepted token first — Chrome's `text/x-ruby-script` for `.rb` becomes `text/x-ruby`, which is what previously produced a permanent 400.
- Text-like uploads smaller than 1 MB are inlined as ordinary text ahead of native forwarding, because inlining cannot be rejected by a provider MIME allowlist. Spreadsheets keep the native path so spreadsheet-specific provider handling still applies, and larger text files and binary documents travel natively.
- Providers without an implemented native file path do not forward native file parts; they use text fallback/marker behavior instead.
- Anything the policy rejects falls back to a notice naming the saved local path. Text-like files (`txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`, and common code files) up to `max_text_embed_bytes` are still embedded as ordinary text, wrapped in explicit begin/end file markers.

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

The built-in `chatgpt` text provider enables Responses WebSockets by default. `openai` defaults to HTTP/SSE and can opt in with `providers.openai.use_websocket: true`. The WebSocket path keeps a persistent connection and, when safe, continues turns with `previous_response_id` plus only the new user/tool input. If the WebSocket connect/write step fails, term-llm falls back to HTTP/SSE; if a WebSocket continuation is rejected because the prior response state is unavailable, it retries that turn once with full state.

Use `false` to keep OpenAI on HTTP/SSE or to disable ChatGPT WebSockets:

```yaml
providers:
  openai:
    use_websocket: false
  chatgpt:
    use_websocket: false
```

OpenAI-compatible providers (`type: openai_compatible`, including local/self-hosted endpoints and OpenRouter-style compatible APIs) do **not** enable WebSockets by default. Their generic adapter uses HTTP/SSE; setting `use_websocket` does not turn an arbitrary compatible endpoint into a native Responses transport.

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

For vLLM templates that use `chat_template_kwargs.thinking`, declare model capabilities in config and set `vllm_thinking_param: thinking`. term-llm treats that metadata as authoritative: it exposes only the declared effort profiles, uses the configured default for the bare provider/model, and passes enabled efforts through unchanged to vLLM.

```yaml
providers:
  cdck_deepseek:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: deepseek-ai/DeepSeek-V4-Flash
    vllm_thinking_param: thinking
    models:
      - id: deepseek-ai/DeepSeek-V4-Flash
        alias: deepseek-v4-flash
        reasoning_efforts: [none, low, high, max]
        default_reasoning_effort: high
```

With this configuration, the bare `cdck_deepseek` profile uses `high`. Shell completion for `-p cdck_deepseek-<TAB>` offers only `none`, `low`, `high`, and `max`. `none` sends `thinking=false` and omits top-level `reasoning_effort`; every enabled level sends `thinking=true` and the exact configured value as top-level `reasoning_effort`. These requests do not send `thinking_token_budget`.

Model names containing `deepseek` still select the `thinking` request shape automatically for backward compatibility. Explicit `vllm_thinking_param` is preferred for aliased deployments because capability and default selection remain config-driven.

term-llm persists streamed reasoning and replays it as assistant `reasoning` on future vLLM turns so vLLM's chat template and prefix cache can see the same prior reasoning. vLLM may still report `reasoning_tokens: 0` in usage metadata even when reasoning text is present; that is a vLLM accounting limitation.

## Dynamic secrets and endpoints

Keep API keys and tokens out of public config by storing references rather than literal secrets:

- `op://...` reads from 1Password.
- `$(...)` captures a secret-manager command's output, including macOS Keychain and Linux Secret Service lookups.
- `file://path#nested.field` reads a JSON or YAML field from a private file; without a fragment, it reads the whole file as trimmed text.
- `${VAR}` / `$VAR` reads an environment variable in supported config fields.
- `srv://...` discovers an HTTPS endpoint through DNS SRV records.

```yaml
providers:
  production-llm:
    type: vllm
    model: Qwen/Qwen3-30B-A3B
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

Deferred vault, file, command, and DNS references resolve when needed, not when config loads. Endpoint resolution applies to provider `url` and `base_url` and to the additional endpoints listed in the guide. Successful deferred feature-credential lookups are cached; restart long-running processes after rotating secrets.

See **[Secret management](/guides/secret-management/)** for supported settings, MCP-specific syntax, failure and fallback behavior, and complete recipes for 1Password, macOS Keychain, Linux keyrings, and a private `secrets.yml`.

## Live voice

Live voice is off by default. Turn it on, then see [Live voice](/guides/live-voice/) for how calls, delegation, and the control lane behave.

```yaml
live:
  enabled: true
```

| Key                              | Default                                            | Meaning                                                                                                                                                                                                                                                                                                                                           |
| -------------------------------- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `live.enabled`                   | `false`                                            | Master switch; also gates the capability the web UI reads.                                                                                                                                                                                                                                                                                        |
| `live.provider`                  | `chatgpt`                                          | `chatgpt` uses OAuth; `openai` and `gemini` use their public APIs with server-side API keys.                                                                                                                                                                                                                                                      |
| `live.instructions`              | _(unset)_                                          | Replaces the conversational prompt; host capability/session context is still appended.                                                                                                                                                                                                                                                            |
| `live.control_plane`             | `off`                                              | Router selector: `off`, `agent`, or `classify`. Booleans remain compatible (`true=agent`, `false=off`). `agent` runs tool-calling triage for session/call/voice controls. `classify` uses `classify.providers` for intent routing and invokes the restricted agent resolver only for confident switches. |
| `live.classify.provider`         | _(unset)_                                          | Classification provider key; inherits `classify.default_provider`. With `control_plane: classify`, an unresolvable provider or credential is a startup configuration error; a `SIGUSR2` process reload repeats that validation before accepting live sessions. |
| `live.classify.log_decisions`    | `true`                                             | Persist `route_decisions` audit rows for `term-llm live decisions [--since]`. Rows persist until the diagnostics `live.db` file is deleted; there is no automatic pruning. |
| `live.classify.log_state`        | `true`                                             | Include bounded speech and session-title state in decision rows. `false` keeps probabilities, labels, outcomes, latency, and bounded error categories but omits state JSON. |
| `live.classify.min_confidence.status` | `0.60`                                        | Minimum probability for a host-rendered running/recent status answer. Finite inclusive range `[0,1]`. |
| `live.classify.min_confidence.new_session` | `0.70`                                   | Minimum probability for starting a fresh conversation; mixed navigation-and-work requests still pass through unchanged. Finite inclusive range `[0,1]`. |
| `live.classify.min_confidence.switch_session` | `0.70`                                | Minimum probability for invoking the restricted switch resolver; mixed requests still pass through unchanged. Finite inclusive range `[0,1]`. |
| `live.classify.min_confidence.steer_now` | `0.80`                                      | Reserved and validated for Phase 2. Phase 1 always maps this label to ordinary `steer`. |
| `live.classify.min_confidence.side` | `0.85`                                           | Reserved and validated for Phase 2. Phase 1 always maps this label to ordinary `steer`. |
| `live.control_provider`          | _(unset)_                                          | Provider for the `agent` backend and for `classify`'s restricted switch resolver. Unset follows the bound session provider's fast model. |
| `live.control_model`             | _(unset)_                                          | Model for the agent triage/switch-resolver turn. Naming a model alone keeps provider resolution on the bound session provider, falling back to the global default. |
| `live.idle_timeout`              | `10m`                                              | Idle time before the server closes a call.                                                                                                                                                                                                                                                                                                        |
| `live.openai.api_key`            | _(unset)_                                          | OpenAI API key; falls back to `OPENAI_API_KEY`.                                                                                                                                                                                                                                                                                                   |
| `live.openai.model`              | `gpt-live-1`                                       | Public GPT-Live model; explicitly named `realtime` models select the legacy Realtime transport.                                                                                                                                                                                                                                                   |
| `live.openai.voice`              | `marin`                                            | Live: `alloy`, `ash`, `ballad`, `beacon`, `bossa`, `cedar`, `cinder`, `coral`, `delta`, `echo`, `gleam`, `marin`, `meridian`, `quartz`, `ripple`, `sage`, `shimmer`, `stone`, `tempo`, `verse`, `vesper`, or `willow`. Legacy Realtime supports only `alloy`, `ash`, `ballad`, `coral`, `echo`, `sage`, `shimmer`, `verse`, `marin`, and `cedar`. |
| `live.openai.base_url`           | `https://api.openai.com/v1`                        | Public API base for call creation and sideband.                                                                                                                                                                                                                                                                                                   |
| `live.gemini.api_key`            | _(unset)_                                          | Gemini API key; falls back to `GEMINI_API_KEY`, then `GOOGLE_API_KEY`.                                                                                                                                                                                                                                                                            |
| `live.gemini.model`              | `gemini-3.8-live`                                  | Gemini Live model used by `BidiGenerateContent`.                                                                                                                                                                                                                                                                                                  |
| `live.gemini.voice`              | `Kore`                                             | Prebuilt Gemini voice. Common choices include `Kore`, `Puck`, `Charon`, `Fenrir`, `Aoede`, `Leda`, `Orus`, and `Zephyr`.                                                                                                                                                                                                                          |
| `live.gemini.base_url`           | Gemini `v1beta` `BidiGenerateContent` WSS endpoint | Advanced proxy/test override.                                                                                                                                                                                                                                                                                                                     |
| `live.chatgpt.model`             | `gpt-live-1-codex`                                 | Voice model for the V3/frameless protocol.                                                                                                                                                                                                                                                                                                        |
| `live.chatgpt.voice`             | `cove`                                             | V3 voice: `juniper`, `maple`, `spruce`, `ember`, `vale`, `breeze`, `arbor`, `sol`, or `cove`; GPT-Live-1.1 adds `viola`, `rio` (pt-BR), `aube`, `rivage` (fr), `sonna`, `miko` (de), `leher`, `neer` (hi), `himari`, `rindo` (ja), `haetsal`, `hanul` (ko), `tinta`, `arrullo` (es-MX) and `bubbie` (en). |
| `live.chatgpt.call_base_url`     | `https://chatgpt.com/backend-api/codex`            | Call-creation endpoint; overridden in tests.                                                                                                                                                                                                                                                                                                      |
| `live.chatgpt.sideband_base_url` | `https://api.openai.com/v1`                        | Control-channel endpoint; overridden in tests.                                                                                                                                                                                                                                                                                                    |

For the classify backend, the selected `classify.providers.<name>.timeout_seconds` supplies the classifier deadline. The live router substitutes a three-second deadline for the provider type's built-in default and clamps longer explicit values to the remaining controller budget (currently 13 seconds), preserving room for the 30-second switch resolver and a two-second margin inside the 45-second route backstop.

Every classify delegation sends the selected classification provider the delegation message, up to 4,096 runes of transcript delta, up to 12 recent session titles, and bounded active-run metadata.

**For `chatgpt` only: do not use the V2 voices `marin` or `cedar` with this protocol.** ChatGPT V3 uses the V1 voice family, not the V2 family. Sending `marin` to this endpoint was verified to return the misleading `403 "Voice session access denied."`; changing only the voice to `cove` allowed the same account and direct client to connect and receive audio. term-llm validates the voice before making a request. If an older configuration explicitly sets `marin`, remove that override or select a supported voice.

`live.enabled` alone is not enough for the button to appear: `/v1/capabilities` reports `live.enabled` only when the feature is on, the request is first-party, and provider readiness succeeds. Account or workspace restrictions may still prevent a call; valid ChatGPT credentials alone do not guarantee model access.

Live voice needs a secure context (HTTPS or localhost) because the browser will not grant microphone access otherwise.

## Skills config

```yaml
skills:
  enabled: true
  auto_invoke: true
  metadata_budget_tokens: 8000
  max_visible_skills: 50
  include_project_skills: true
  allow_project_script_tools: false
  include_ecosystem_paths: true
  always_enabled: [git, code-review]
  never_auto: [expensive-api-skill]
```

Controls the skills system: portable instruction bundles that inject task-specific context into the system prompt. Skills and model-driven activation are enabled by default. Set `skills.enabled: false` to disable the system in the global configuration (unless overridden by an agent or CLI flag), or use `--skills none` to disable it for one invocation. See [Skills](/guides/skills/) for the full guide.

Project skill-declared script tools are disabled by default. Set `skills.allow_project_script_tools: true` in user config, or pass `--allow-project-skill-tools` to `chat`, `ask`, or `serve`, to enable their execution without approval. An explicit `--allow-project-skill-tools=false` overrides a `true` user setting; an absent flag preserves config. Yolo mode always bypasses this execution gate. User-installed skill tools and agent-defined custom tools are unchanged. This gate does not send scripts to Guardian or change ordinary shell approvals. It does not override `--skills none` or `include_project_skills: false`.

## Diagnostics

```yaml
diagnostics:
  enabled: true
```

When edit retries fail, diagnostics can capture prompts, partial responses, and failure context for inspection.

## Related pages

- [Secret management](/guides/secret-management/)
- [Providers and models](/reference/providers-and-models/)
- [Search](/guides/search/)
- [Sessions](/reference/sessions/)
- [Skills](/guides/skills/)
- [Text embeddings](/guides/text-embeddings/)

### Agent spawn depth

In an agent's `agent.yaml`, `spawn.max_depth` sets the number of levels **below that agent** it may spawn (`0` or unset uses the default `2`). A child starts with `min(child.spawn.max_depth, max(parent_remaining - 1, 0))` remaining levels, so a child's own setting cannot expand its caller's budget. A top-level `developer` with `max_depth: 1` can spawn one child; a `developer` spawned by a parent with two levels remaining can also spawn one child, but its child cannot spawn again. Resumed children and queued children relaunched with `continue_agent` take the cap from the resuming parent. Isolated skill children are exempt from the agent spawn limit: they can run even at zero remaining budget, but inherit `max(parent_remaining - 1, 0)` and cannot spawn agents at zero.

### Host-scoped agents

An agent bundle may set `workspace: none` to retain its execution directory without treating it as a primary workspace. This suppresses primary-workspace confirmation and `manage_workspace`, but does not remove independent read, write, shell, project, Guardian, or yolo authority.
