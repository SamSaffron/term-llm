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
term-llm models --provider nearai
term-llm models --provider sambanova
term-llm models --provider ollama
term-llm models --json
```

Use `providers` when you want to know what is available and how it is configured. Use `models` when you want the concrete model names a provider currently exposes.

## Provider categories

term-llm supports a mix of provider types:

- hosted API providers such as Anthropic, AWS Bedrock, OpenAI, xAI, Gemini, NEAR AI Cloud, SambaNova, and OpenRouter
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
| `venice` | `VENICE_API_KEY` | Venice OpenAI-compatible API key |
| `nearai` | `NEARAI_API_KEY` | NEAR AI Cloud OpenAI-compatible TEE inference key |
| `sambanova` | `SAMBANOVA_API_KEY` | SambaNova Cloud OpenAI-compatible API key |
| `openrouter` | `OPENROUTER_API_KEY` | OpenRouter API key |
| `vllm` / custom `type: vllm` entries | `VLLM_API_KEY` or `<PROVIDER_NAME>_API_KEY` | Optional for unauthenticated local servers; vLLM OpenAI-compatible API plus reasoning controls for supported chat templates |
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

## GPT-5.6 on OpenAI and ChatGPT

term-llm ships fallback metadata for the GPT-5.6 family:

| Provider | Default model | Fast model | Effective input | Max output | Efforts |
|---|---|---|---:|---:|---|
| `openai` (API key) | `gpt-5.6-sol` | `gpt-5.6-luna` | 922K | 128K | `none`, `low`, `medium`, `high`, `xhigh`, `max` |
| `chatgpt` (OAuth) | `gpt-5.6-sol-medium` | `gpt-5.6-luna` | 372K fallback | 128K | Sol/Terra: `low` through `max`, plus `ultra`; Luna: `low` through `max` |

The OpenAI model IDs are `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`. ChatGPT exposes the same named variants through its live Codex catalog; live input limits and reasoning metadata take precedence over the static fallback. Luna keeps its upstream medium default unless you explicitly select another effort.

**Pro and Ultra are different controls.** On the public OpenAI API, Pro sends `reasoning.mode: pro`; Standard sends `reasoning.mode: standard`. In terminal chat use `/pro on`, `/pro off`, or `/pro status`. The browser shows a Standard/Pro selector only when the selected model advertises it. ChatGPT OAuth does not receive public-API Pro mode: its `ultra` option is a distinct reasoning effort available only on Sol/Terra.

OpenAI API GPT-5.6 also supports term-llm's advanced Responses controls: reasoning context, hosted multi-agent execution, programmatic tool calling, and prompt-cache options. These controls are model/provider gated. term-llm rejects them for older OpenAI models, OpenAI-compatible providers, and the ChatGPT OAuth backend rather than sending unverified fields. See [Configuration](/reference/configuration/#gpt-56-advanced-responses-controls) and [Web UI and API](/guides/web-ui-and-api/#gpt-56-responses-controls).

Bundled OpenAI API prices per 1M tokens (uncached input / cache read / cache write / output) are Sol: `$5 / $0.50 / $6.25 / $30`; Terra: `$2.50 / $0.25 / $3.125 / $15`; Luna: `$1 / $0.10 / $1.25 / $6`. When total input (uncached + cache reads + cache writes) exceeds 272K, the entire request uses 2× input/read/write rates and 1.5× output rates. This is API-key billing metadata; ChatGPT subscription access is not billed from these static prices.

## SambaNova Cloud

SambaNova is available as a built-in OpenAI-compatible provider:

```bash
export SAMBANOVA_API_KEY=your-key
term-llm ask --provider sambanova:gpt-oss-120b "quick question"
term-llm models --provider sambanova
```

```yaml
providers:
  sambanova:
    model: gpt-oss-120b
    fast_model: Meta-Llama-3.3-70B-Instruct
```

The provider uses `https://api.sambanova.ai/v1`, supports tool calls, and has a curated fallback model list. `term-llm models --provider sambanova` queries SambaNova's `/models` endpoint; because the OpenAI-compatible model response does not generally include price metadata, term-llm annotates known SambaNova models with bundled public prices from `https://cloud.sambanova.ai/plans/pricing`. These prices are also used by `term-llm usage` cost calculation for matching SambaNova model IDs.

## NEAR AI Cloud

NEAR AI Cloud is available as a built-in OpenAI-compatible provider for TEE-backed private inference:

```bash
export NEARAI_API_KEY=your-key
term-llm ask --provider nearai:zai-org/GLM-5.1-FP8 "quick question"
term-llm models --provider nearai
```

```yaml
providers:
  nearai:
    model: zai-org/GLM-5.1-FP8
    fast_model: Qwen/Qwen3.6-35B-A3B-FP8
```

The provider uses `https://cloud-api.near.ai/v1`, supports tool calls, and has a curated fallback list of TEE-hosted text models. `term-llm models --provider nearai` queries NEAR AI Cloud's public `/model/list` catalog and filters it to chat-capable models, with token prices shown per 1M tokens when available.

## vLLM reasoning providers

For vLLM servers running reasoning models, prefer `type: vllm` instead of generic `openai_compatible`. The vLLM provider still uses the OpenAI-compatible `/v1/chat/completions` API, but maps term-llm reasoning effort suffixes into model-family-specific vLLM request fields and replays prior assistant reasoning with vLLM's current `reasoning` message field.

For Qwen-family models, term-llm sends Qwen chat-template controls:
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

Use the normal provider flag, or append an effort suffix to the configured provider name:

```bash
term-llm ask -p cdck_qwen "hello"          # default: no thinking
term-llm ask -p cdck_qwen-low "harder"     # thinking budget 1024
term-llm ask -p cdck_qwen-medium "hard"    # thinking budget 4096
term-llm ask -p cdck_qwen-high "very hard" # thinking budget 10000
```

The effort suffix is stripped before sending the model name upstream. For example, `-p cdck_qwen-high` sends `model: Qwen/Qwen3.5-122B-A10B` plus Qwen thinking controls, not a literal `Qwen/Qwen3.5-122B-A10B-high` model ID.

| Effort | Request fields sent to vLLM |
|---|---|
| default / empty | `chat_template_kwargs.enable_thinking: false`; no `thinking_token_budget` |
| `low` | `enable_thinking: true`, `thinking_token_budget: 1024` |
| `medium` | `enable_thinking: true`, `thinking_token_budget: 4096` |
| `high` / `xhigh` / `max` | `enable_thinking: true`, `thinking_token_budget: 10000` |

For DeepSeek models served by vLLM, the official recipes use a different chat-template shape. term-llm auto-detects model names containing `deepseek`; if your provider/model is mistitled, set `vllm_thinking_param: thinking` explicitly:

```yaml
providers:
  cdck_deepseek:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: ds31                    # served-model alias; actual backend is DeepSeek
    vllm_thinking_param: thinking  # use DeepSeek/vLLM chat_template_kwargs.thinking
```

DeepSeek effort mapping follows the official DeepSeek/vLLM behavior: DeepSeek exposes non-think, Think High, and Think Max rather than Qwen-style token budgets.

| term-llm effort | Request fields sent to vLLM for DeepSeek |
|---|---|
| default / empty / `minimal` / `none` | `chat_template_kwargs.thinking: false` |
| `low` / `medium` / `high` | `chat_template_kwargs.thinking: true`, `chat_template_kwargs.reasoning_effort: high` |
| `xhigh` / `max` | `chat_template_kwargs.thinking: true`, `chat_template_kwargs.reasoning_effort: max` |

DeepSeek requests do not send `thinking_token_budget`. The `reasoning_effort` value is nested inside `chat_template_kwargs`, matching vLLM's DeepSeek recipes, rather than sent as top-level OpenAI `reasoning_effort`.

Notes:

- Start vLLM with the appropriate reasoning parser for your model, for example `--reasoning-parser qwen3` for Qwen3-family reasoning output or `--reasoning-parser deepseek_v3` / `deepseek_v4` for DeepSeek variants.
- Qwen `thinking_token_budget` requires a vLLM server new enough to support it and, on recent vLLM, a server-side `--reasoning-config`. Plain/default Qwen requests omit the budget so they work without that extra server option.
- vLLM currently streams reasoning text in `delta.reasoning`, but may not report `usage.completion_tokens_details.reasoning_tokens` accurately. In that case term-llm can show reasoning in debug output while `reasoning_tokens` remains `0`; this reflects vLLM usage metadata, not missing reasoning text.
- For multi-turn conversations, term-llm persists streamed reasoning and replays it as assistant `reasoning` on the next vLLM request. This lets vLLM's chat template render the prior reasoning consistently and gives prefix caching the best chance to reuse shared prompt prefixes when the server has prefix caching enabled.

## OpenAI-compatible providers

For local or custom backends that do not need vLLM chat-template thinking controls, use `type: openai_compatible`.

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
| `type` | string | Use `openai_compatible` for generic custom providers, or `vllm` for vLLM servers that should receive reasoning controls for Qwen/DeepSeek-style chat templates. Inferred automatically for known names like `ollama`, `cerebras`, `groq`, and `vllm`. |
| `base_url` | string | Base URL (e.g., `http://localhost:11434/v1`). `/chat/completions` is appended automatically. |
| `url` | string | Full chat completions URL, used as-is. Use this when your endpoint path differs from the standard. Supports `srv://` for DNS SRV discovery and `$()` for command-based resolution. |
| `api_key` | string | API key. Supports `${ENV_VAR}`, `op://`, `file://`, and `$()` resolution. If omitted, term-llm tries `<PROVIDER_NAME>_API_KEY` from the environment. |
| `model` | string | Default model name. For configured model objects, this may be either the upstream `id` or the friendly `alias`. |
| `models` | list | Optional list for model pickers and shell completion. Entries may be strings or objects with `id`, optional `alias`, `context_window`, `max_output_tokens`, `parse_reasoning`, `include_reasoning`, `thinking_param`, and `reasoning_efforts`. |
| `fast_model` | string | Lightweight model used for control-plane tasks (e.g., title generation) and the agent `model: fast` alias. This is separate from service-tier fast mode. Usually this is all you need. |
| `fast_provider` | string | Optional provider key to use when the `fast_model` should run on a different configured provider than this one. |
| `service_tier` | string | Optional Responses API service tier for built-in `openai` and `chatgpt` providers. Use `fast` or `priority` to request fast/priority service where the selected model supports it. Omit the field to send no service tier. |
| `context_window` | int | Override context window size in tokens. Use this for self-hosted models not in the built-in token limit tables. |
| `max_output_tokens` | int | Override maximum output tokens. Same use case as `context_window`. |
| `no_stream_options` | bool | When `true`, don't send `stream_options` in the request. Use this for servers that reject the field. Default `false`; most OpenAI-compatible servers (vLLM, Ollama, LM Studio) support it and need it to report token usage. |
| `parse_reasoning` | bool | Send `parse_reasoning` for OpenAI-compatible APIs that can parse inline model thinking into `reasoning_content` (for example Friendli). |
| `include_reasoning` | bool | Send `include_reasoning`; useful with `parse_reasoning: true` when you want streamed `delta.reasoning_content` events. |
| `thinking_param` | string | Generic OpenAI-compatible chat-template control. When a reasoning effort is selected (for example a `-high`/`-max` suffix), term-llm sends `chat_template_kwargs.<thinking_param>: true`. Friendli GLM-5.2 uses `enable_thinking`. |
| `vllm_thinking_param` | string | `type: vllm` only. Override the chat-template thinking key when auto-detection is not possible: `enable_thinking` for Qwen-style templates, `thinking` for DeepSeek-style templates. |
| `use_websocket` | bool | Reserved for providers with native Responses WebSocket support. Defaults to `true` only for built-in `openai` and `chatgpt`; OpenAI-compatible providers default to HTTP/SSE. |

### Model object entries

`models` may mix plain strings and objects. Plain strings are enough for autocomplete/model picker entries. Object entries are for endpoints where the model you want to type locally differs from the model ID the API expects, or where each model needs its own metadata.

```yaml
providers:
  custom:
    type: openai_compatible
    base_url: https://api.example.com/v1
    api_key: ${CUSTOM_API_KEY}
    model: friendly-name
    # Optional provider-level default for all models under this provider.
    # The target may be provider:model, or just provider to use that provider's default model.
    vision_via: gemini
    models:
      - simple-upstream-model
      - id: upstream/model-id
        alias: friendly-name
        context_window: 262144
        max_output_tokens: 32768
        # Optional: only set these for APIs/models that support them.
        parse_reasoning: true
        include_reasoning: true
        thinking_param: enable_thinking
        reasoning_efforts: [high, max]
        # Optional per-model override; if omitted, provider-level vision_via is used.
        vision_via: gemini:gemini-2.5-pro
```

| Model object field | Description |
|---|---|
| `id` | Upstream model ID sent in the API request. If `alias` is omitted, this is also the local name. |
| `alias` | Friendly local name for CLI use, shell completion, and model picker display. The provider default `model` may be either `id` or `alias`. |
| `context_window` | Per-model context window metadata. |
| `max_output_tokens` | Per-model output token cap metadata; OpenAI-compatible requests clamp explicit `max_output_tokens` to this value. |
| `parse_reasoning` | Per-model override for the provider-level `parse_reasoning` flag. |
| `include_reasoning` | Per-model override for the provider-level `include_reasoning` flag. |
| `thinking_param` | Per-model override for the provider-level `thinking_param` key. Sent as `chat_template_kwargs.<thinking_param>: true` only when a non-default effort is selected. |
| `reasoning_efforts` | Exact suffixes to expose for this model, for example `[high, max]`. The bare model/alias remains the default and sends no `reasoning_effort`. |
| `vision_via` | Optional `provider` or `provider:model` route for indirect image understanding. Can be set at provider level (`providers.<name>.vision_via`) as the default for all models on that provider, or on an individual model object to override the provider default. If only `provider` is given, that provider's configured default `model` is used. When set, uploaded image parts are replaced with local path references for the primary model, `view_image` is auto-enabled, and that tool asks the configured vision model to return a text-only analysis. Requires the primary model to support tool calls and the vision provider credentials to be configured. |

For `-p custom:friendly-name-max`, term-llm sends `model: upstream/model-id` plus `reasoning_effort: max`. If `reasoning_efforts` is empty or omitted, no effort-suffixed aliases are generated for that model.

### Friendli reasoning

Friendli is OpenAI-compatible, but reasoning-capable models such as GLM-5.2 need explicit parser flags to expose thinking as `reasoning_content` instead of leaving it inline. Configure those fields on the specific model entry:

```yaml
providers:
  friendli:
    type: openai_compatible
    base_url: https://api.friendli.ai/serverless/v1
    api_key: ${FRIENDLI_API_KEY}
    model: glm52
    models:
      - id: zai-org/GLM-5.2
        alias: glm52
        context_window: 1048576
        max_output_tokens: 131072
        parse_reasoning: true
        include_reasoning: true
        thinking_param: enable_thinking
        reasoning_efforts: [high, max]
```

With that config, effort suffixes are generated only from the declared `reasoning_efforts`. For example `-p friendli:glm52-max` sends `model: zai-org/GLM-5.2`, `reasoning_effort: max`, `parse_reasoning: true`, `include_reasoning: true`, and `chat_template_kwargs.enable_thinking: true`. The bare `-p friendli:glm52` sends no `reasoning_effort`; because only `high` and `max` are listed, term-llm does not offer `glm52-low` or `glm52-medium` completions.

You can set the same reasoning-parser fields at the provider level as defaults, then override them per model object when different models on the same endpoint need different behavior.

### Full example

```yaml
providers:
  my-vllm:
    type: vllm
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

## Service tiers and fast mode

Built-in `openai` and `chatgpt` text providers can send the Responses API `service_tier` field. To request fast/priority service for all turns through a provider, set `service_tier` in that provider config:

```yaml
providers:
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
    service_tier: fast      # alias for API value "priority"

  chatgpt:
    model: gpt-5.6-sol-medium
    fast_model: gpt-5.6-luna
    service_tier: priority  # equivalent to "fast"
```

Leave `service_tier` unset to omit the field entirely. Only some models/accounts support fast service; unsupported requests may be ignored or rejected by the provider. In chat, `/fast` toggles the fast service tier for the current session. The status line shows `fast` when it is currently active.

This is different from `fast_model` / optional `fast_provider`, which choose a lightweight model for term-llm control-plane tasks such as summaries or title generation, and for agent configs that use `model: fast`.

## Reasoning and model suffixes

Model/provider suffixes control how much reasoning a provider is asked to do. Display of the resulting reasoning is controlled separately by the top-level [`reasoning`](/reference/configuration/#reasoning-and-thinking-display) config. Non-encrypted provider-marked thinking is shown as collapsed `Thinking...` / `Thought: <title>` blocks by default; encrypted reasoning/signature payloads are replay-only and are never displayed.

### OpenAI reasoning effort

For GPT-5.6 on the OpenAI API, append `-none`, `-low`, `-medium`, `-high`, `-xhigh`, or `-max` to control reasoning effort. An explicit `reasoning_effort` API field wins over a model suffix, and a request suffix wins over a provider-level default.

```bash
term-llm ask --provider openai:gpt-5.6-sol-xhigh "complex question"
term-llm exec --provider openai:gpt-5.6-luna-low "quick task"
```

```yaml
providers:
  openai:
    model: gpt-5.6-sol-high
```

| Effort | Meaning |
|---|---|
| `none` | no reasoning effort on GPT-5.6 |
| `low` | faster, cheaper, less thorough |
| `medium` | balanced upstream default |
| `high` | more thorough reasoning |
| `xhigh` | very high reasoning |
| `max` | maximum GPT-5.6 reasoning |

Older GPT-5 models retain their existing provider-specific effort sets. Natural model IDs ending in a suffix-like word are preserved when that suffix is not valid for the model; for example, `gpt-5.1-codex-max` remains a model ID rather than being split into model plus `max` effort.

For ChatGPT OAuth, GPT-5.6 Sol and Terra additionally expose `ultra`; Luna does not. Ultra is an effort, not the public OpenAI API's Pro reasoning mode.

### vLLM thinking suffixes

For configured providers with `type: vllm`, the same suffix parser can be applied to the provider name itself. This is useful when the model ID is long and already configured:

```bash
term-llm ask -p cdck_qwen-high "reason carefully"
```

With:

```yaml
providers:
  cdck_qwen:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: Qwen/Qwen3.5-122B-A10B
```

term-llm sends the base model ID plus vLLM thinking controls. For Qwen-style templates:

| Suffix | Qwen/vLLM behavior |
|---|---|
| none | disable thinking by default (`enable_thinking: false`), no `thinking_token_budget` |
| `-low` | enable thinking, budget `1024` |
| `-medium` | enable thinking, budget `4096` |
| `-high` / `-xhigh` / `-max` | enable thinking, budget `10000` |

For DeepSeek-style templates, auto-detected from `deepseek` in the model name or forced with `vllm_thinking_param: thinking`:

| Suffix | DeepSeek/vLLM behavior |
|---|---|
| none | `thinking: false` |
| `-low` / `-medium` / `-high` | `thinking: true`, `reasoning_effort: high` |
| `-xhigh` / `-max` | `thinking: true`, `reasoning_effort: max` |

Reasoning replay uses vLLM's `reasoning` assistant-message field. vLLM may still report `reasoning_tokens: 0` in usage metadata even when reasoning text was streamed; this is a known vLLM-side accounting gap.

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
