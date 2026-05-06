---
title: "Providers and models"
weight: 3
description: "Choose providers, discover models, understand credentials, and use provider-specific model features such as reasoning and native search."
kicker: "Providers"
featured: true
next:
  label: Sessions
  url: /reference/sessions/
---
## Discover providers and models

```bash
term-llm providers
term-llm providers --configured
term-llm providers anthropic

term-llm models --provider anthropic
term-llm models --provider openrouter
term-llm models --provider ollama
term-llm models --json
```

Use `providers` when you want to know what is available and how it is configured. Use `models` when you want the concrete model names a provider currently exposes.

## Provider categories

term-llm supports a mix of provider types:

- hosted API providers such as Anthropic, AWS Bedrock, OpenAI, xAI, Gemini, and OpenRouter
- subscription-backed OAuth providers such as ChatGPT, Copilot, and Gemini CLI
- local or self-hosted OpenAI-compatible providers such as Ollama, LM Studio, vLLM, or custom endpoints

## Credentials

Most providers use API keys via environment variables. Some use OAuth credentials from companion CLIs or locally stored auth files.

| Provider | Credentials source | Notes |
|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` | API key |
| `bedrock` | AWS credential chain or explicit `access_key_id` / `secret_access_key` | Anthropic Claude via AWS Bedrock |
| `openai` | `OPENAI_API_KEY` | Standard OpenAI API key |
| `chatgpt` | `~/.config/term-llm/chatgpt_creds.json` | ChatGPT Plus/Pro OAuth |
| `copilot` | `~/.config/term-llm/copilot_creds.json` | GitHub Copilot OAuth |
| `gemini` | `GEMINI_API_KEY` | Google AI Studio key |
| `gemini-cli` | `~/.gemini/oauth_creds.json` | gemini-cli OAuth |
| `xai` | `XAI_API_KEY` | xAI API key |
| `openrouter` | `OPENROUTER_API_KEY` | OpenRouter API key |
| `zen` | `ZEN_API_KEY` optional | empty is valid for free tier |

Examples:

```bash
term-llm ask --provider anthropic "question"
term-llm ask --provider chatgpt "question"
term-llm ask --provider copilot "question"
term-llm ask --provider gemini-cli "question"
```

## WebSocket defaults

The built-in `openai` and `chatgpt` text providers use the Responses WebSocket transport by default. This improves latency in agentic/tool-heavy runs by reusing one connection and continuing compatible turns with `previous_response_id` plus only new input. If setup fails before streaming starts, term-llm falls back to HTTP/SSE; if a WebSocket continuation rejects the previous response ID, it retries once with full input.

To force HTTP/SSE for either built-in provider:

```yaml
providers:
  openai:
    use_websocket: false
  chatgpt:
    use_websocket: false
```

OpenAI-compatible providers remain HTTP/SSE by default. WebSocket defaults are not applied to `type: openai_compatible` entries.

## OpenAI-compatible providers

For local or custom backends, use `type: openai_compatible`.

```yaml
providers:
  ollama:
    type: openai_compatible
    base_url: http://localhost:11434/v1
    model: llama3.2:latest

  lmstudio:
    type: openai_compatible
    base_url: http://localhost:1234/v1
    model: deepseek-coder-v2

  cerebras:
    type: openai_compatible
    base_url: https://api.cerebras.ai/v1
    model: llama-4-scout-17b
    api_key: ${CEREBRAS_API_KEY}
```

Use `base_url` when the standard `/chat/completions` path should be appended automatically. Use `url` when you need to specify the full chat completions endpoint directly.

### Configuration reference

| Field | Type | Description |
|---|---|---|
| `type` | string | Must be `openai_compatible` for custom providers. Inferred automatically for known names like `ollama`, `cerebras`, `groq`. |
| `base_url` | string | Base URL (e.g., `http://localhost:11434/v1`). `/chat/completions` is appended automatically. |
| `url` | string | Full chat completions URL, used as-is. Use this when your endpoint path differs from the standard. Supports `srv://` for DNS SRV discovery and `$()` for command-based resolution. |
| `api_key` | string | API key. Supports `${ENV_VAR}`, `op://`, `file://`, and `$()` resolution. If omitted, term-llm tries `<PROVIDER_NAME>_API_KEY` from the environment. |
| `model` | string | Default model name sent to the server. |
| `models` | list | Optional list of model names for shell tab completion with `--provider name:<TAB>`. |
| `fast_model` | string | Lightweight model used for control-plane tasks (e.g., title generation). |
| `fast_provider` | string | Provider key to use for `fast_model` if it lives on a different provider. |
| `context_window` | int | Override context window size in tokens. Use this for self-hosted models not in the built-in token limit tables. |
| `max_output_tokens` | int | Override maximum output tokens. Same use case as `context_window`. |
| `no_stream_options` | bool | When `true`, don't send `stream_options` in the request. Use this for servers that reject the field. Default `false`; most OpenAI-compatible servers (vLLM, Ollama, LM Studio) support it and need it to report token usage. |
| `use_websocket` | bool | Reserved for providers with native Responses WebSocket support. Defaults to `true` only for built-in `openai` and `chatgpt`; OpenAI-compatible providers default to HTTP/SSE. |

### Full example

```yaml
providers:
  my-vllm:
    type: openai_compatible
    base_url: http://gpu-server:8000/v1
    model: Qwen/Qwen3-30B-A3B
    api_key: ${VLLM_API_KEY}
    context_window: 32768
    max_output_tokens: 8192
    models:
      - Qwen/Qwen3-30B-A3B
      - Qwen/Qwen3-8B

  legacy-server:
    type: openai_compatible
    url: http://old-server:5000/api/chat
    model: custom-finetune
    no_stream_options: true  # this server rejects stream_options
```

## Reasoning and model suffixes

### OpenAI reasoning effort

For OpenAI models, append `-low`, `-medium`, `-high`, or `-xhigh` to control reasoning effort.

```bash
term-llm ask --provider openai:gpt-5.2-xhigh "complex question"
term-llm exec --provider openai:gpt-5.2-low "quick task"
```

```yaml
providers:
  openai:
    model: gpt-5.2-high
```

| Effort | Meaning |
|---|---|
| `low` | faster, cheaper, less thorough |
| `medium` | balanced default |
| `high` | more thorough reasoning |
| `xhigh` | maximum reasoning on supported models |

### Anthropic extended thinking

For Anthropic models, append `-thinking`:

```bash
term-llm ask --provider anthropic:claude-sonnet-4-6-thinking "complex question"
```

```yaml
providers:
  anthropic:
    model: claude-sonnet-4-6-thinking
```

### AWS Bedrock

The `bedrock` provider routes Anthropic Claude models through AWS Bedrock. It supports the same model suffixes (`-thinking`, `-1m`) and has full feature parity with the direct `anthropic` provider.

**Authentication** uses the standard AWS credential chain (`AWS_ACCESS_KEY_ID` env var, `~/.aws/credentials`, instance profiles), or explicit credentials in config:

```yaml
providers:
  bedrock:
    region: us-west-2
    access_key_id: $(op-cache read "op://Private/AWS Bedrock/AWS_ACCESS_KEY_ID")
    secret_access_key: $(op-cache read "op://Private/AWS Bedrock/AWS_SECRET_ACCESS_KEY")
    model: claude-sonnet-4-6-thinking
```

**Model resolution** uses a 3-tier system. Friendly model names like `claude-sonnet-4-6` are automatically translated to Bedrock cross-region IDs. Use `model_map` to override with application inference profile ARNs or specific Bedrock IDs:

```yaml
providers:
  bedrock:
    region: us-west-2
    model: claude-sonnet-4-6-thinking
    model_map:
      claude-sonnet-4-6: arn:aws:bedrock:us-west-2:123456789:application-inference-profile/abc123
      claude-opus-4-6: us.anthropic.claude-opus-4-6-v1
```

Suffixes are stripped before lookup, so `claude-sonnet-4-6-1m-thinking` strips to `claude-sonnet-4-6`, resolves through `model_map`, then re-applies thinking and 1M context.

The geographic prefix (`us.`, `eu.`, `ap.`) is derived from the configured region automatically. For example, `eu-west-1` produces `eu.anthropic.*` IDs, `ap-southeast-1` produces `ap.anthropic.*`, etc. This ensures data residency matches your region without manual override.

Raw Bedrock model IDs (`us.anthropic.claude-sonnet-4-6`, `anthropic.claude-sonnet-4-6`) and full ARNs are passed through without translation.

| Config field | Description |
|---|---|
| `region` | AWS region. Falls back to `AWS_REGION` env var, then `us-east-1`. |
| `profile` | AWS profile name from `~/.aws/credentials`. |
| `access_key_id` | Explicit AWS access key. Supports `$()`, `op://`, `${ENV}`. |
| `secret_access_key` | Explicit AWS secret key. Same resolution support. |
| `session_token` | Optional session token for temporary credentials. |
| `model_map` | Map of friendly names to Bedrock model IDs or ARNs. |

## Native search support

Some providers support native web search. Others rely on external search tooling.

Native support is most relevant for:

- Anthropic
- Bedrock
- OpenAI
- xAI
- Gemini

You can override behavior with:

```bash
term-llm ask "latest news" -s --native-search
term-llm ask "latest news" -s --no-native-search
```

Or in config:

```yaml
search:
  force_external: true

providers:
  gemini:
    use_native_search: false
```

See [Search](/guides/search/) for the full routing model.

## Recommendations by use case

- **fast free experimentation:** `zen`
- **OpenAI ecosystem / Codex editing:** `openai`
- **Claude models:** `anthropic`
- **Claude models via AWS billing:** `bedrock`
- **broad model access:** `openrouter`
- **local inference:** `ollama` or another OpenAI-compatible endpoint
- **subscription-backed consumer access:** `chatgpt`, `copilot`, or `gemini-cli`

## Related pages

- [Configuration](/reference/configuration/)
- [Search](/guides/search/)
- [Providers and setup](/getting-started/providers-and-setup/)
