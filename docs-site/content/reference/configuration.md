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
- feature-specific blocks such as `image`, `embed`, `search`, `sessions`, and `tools`

## Example

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.2
    credentials: codex

  xai:
    model: grok-4-1-fast

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
  instructions: |
    Be concise. I'm an experienced developer.

edit:
  model: gpt-5.2-codex
  diff_format: auto

search:
  provider: duckduckgo

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
  provider: perplexity
  force_external: false

  perplexity:
    api_key: ${PERPLEXITY_API_KEY}

  exa:
    api_key: ${EXA_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}
```

Search is large enough to deserve its own page; see [Search](/guides/search/).

## Image and embedding config

```yaml
image:
  provider: gemini
  output_dir: ~/Pictures/term-llm

embed:
  provider: gemini
```

Each feature block can hold provider-specific credentials and defaults. The image and embedding providers are independent of the main text provider.

## Provider-specific environment overrides

Providers that shell out to local CLIs can accept extra subprocess environment variables via `providers.<name>.env`.

For `claude-bin`, term-llm also disables Claude Code hooks by default so user-level Claude automation does not leak into inference sessions. Set `providers.claude-bin.enable_hooks: true` if you explicitly want Claude Code hooks to run.

Example for Claude Code when term-llm itself runs as root inside a trusted container:

```yaml
providers:
  claude-bin:
    model: opus
    env:
      IS_SANDBOX: "1"
      CLAUDE_CODE_OAUTH_TOKEN: "file:///root/.config/term-llm/anthropic_oauth.json#access_token"
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

## Dynamic secrets and endpoints

term-llm supports dynamic resolution for some config values:

- `op://...` for 1Password secret references
- `srv://...` for DNS SRV-based endpoint discovery
- `$()` for command-based resolution

Example:

```yaml
providers:
  production-llm:
    type: openai_compatible
    model: Qwen/Qwen3-30B-A3B
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

These values are resolved lazily when term-llm actually needs them.

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
- [Text embeddings](/guides/text-embeddings/)
