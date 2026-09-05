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
term-llm providers                    # built-in and configured provider names
term-llm providers --builtin
term-llm providers --configured
term-llm providers --json
term-llm providers ollama             # details for one provider

term-llm models --provider ollama
term-llm models --provider chatgpt
term-llm models --provider openrouter
term-llm models --provider zen
term-llm models --provider opencode-go --json
```

`providers` describes term-llm's integrations and local configuration. It is not a connectivity or account-entitlement check. `models` queries the selected backend when its adapter supports discovery; the default provider is used when `--provider` is omitted. `gemini`, `bedrock`, and `claude-bin` currently show curated model names rather than a live account catalog. Other adapters may use cached or fallback metadata when offline.

A catalog entry is not a guarantee of capacity, free access, tool support, or access by your account. Use the upstream model ID returned for your provider; the same model family can have different names and limits on different services.

## Provider categories

The provider name selects an integration, not just a model vendor:

- **Hosted APIs:** Anthropic, OpenAI, Gemini, xAI, OpenRouter, Venice, NEAR AI Cloud, SambaNova, and OpenCode Zen.
- **AWS:** Bedrock uses AWS credentials and Bedrock model/inference-profile identifiers.
- **Subscriptions:** ChatGPT, Grok, and Copilot use term-llm-managed OAuth; OpenCode Go uses a subscription API key.
- **Companion CLIs:** `claude-bin`, `cursor-bin`, `agy-bin`, and `grok-bin` run a separately installed and authenticated CLI.
- **Local and self-hosted:** `ollama` uses Ollama's native API. LM Studio and generic endpoints use `openai_compatible`; `vllm` adds model-specific thinking controls to that protocol.

These are **text/agent providers**. [Image](/guides/image-generation/), [audio](/guides/audio-generation/), [music](/guides/music-generation/), [video](/guides/video-generation/), [transcription](/guides/transcription/), and [embedding](/guides/text-embeddings/) commands have their own provider sets and configuration. Support in one command does not imply support in another.

## Credentials

Config paths below are relative to `$XDG_CONFIG_HOME/term-llm/`, normally `~/.config/term-llm/`. Use `term-llm config path` to locate your file. Provider-specific API keys can also be supplied in `providers.<name>.api_key` with the normal [deferred credential resolution](/reference/configuration/).

| Provider | Authentication / setup | Notes |
|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` | Native Anthropic Messages API; also supports compatible gateways. |
| `openai` | `OPENAI_API_KEY` | Public OpenAI Responses API; distinct from a ChatGPT subscription. |
| `chatgpt` | `term-llm auth login chatgpt`; `chatgpt_oauth.json` | Account's ChatGPT/Codex model access and limits apply. |
| `grok` | `term-llm auth login grok`; `grok_oauth.json` | Grok subscription OAuth, not the public xAI API or Grok Build CLI. |
| `copilot` | `term-llm auth login copilot`; `copilot_oauth.json` | GitHub account and organization policy determine available models. |
| `gemini` | `GEMINI_API_KEY` | Google Gemini API. |
| `bedrock` | AWS credential chain or explicit `access_key_id` / `secret_access_key` | Region, profile, model access, and inference-profile routing matter. |
| `openrouter` | `OPENROUTER_API_KEY` | Use OpenRouter's provider-qualified model IDs. |
| `xai` | `XAI_API_KEY` | Public xAI API. |
| `venice` | `VENICE_API_KEY` | Hosted text models and Venice-native search. |
| `nearai` | `NEARAI_API_KEY` | NEAR AI Cloud; check the service's privacy and TEE policies. |
| `sambanova` | `SAMBANOVA_API_KEY` | SambaNova Cloud. |
| `zen` | No key for supported free models; optional `ZEN_API_KEY` | Free model availability changes independently of term-llm releases. |
| `opencode-go` | `OPENCODE_API_KEY` | OpenCode Go subscription key; distinct from Zen's optional key. |
| `ollama` | Running local Ollama server; no API key needed for local use | Native `/api/chat`; configure `base_url` or `OLLAMA_HOST`, without `/v1`. |
| `vllm` | Configured endpoint/model; optional `VLLM_API_KEY` | Whether a key is required depends on your server. |
| `claude-bin` | Installed `claude` CLI and its login | Uses Claude Code's authentication; no separate term-llm API key required. |
| `cursor-bin` | Installed `cursor-agent` CLI and its login, or `CURSOR_API_KEY` | Model availability comes from the companion CLI/account. |
| `agy-bin` | Installed `agy` CLI and its login | Uses Antigravity CLI credentials. |
| `grok-bin` | Installed `grok` CLI; `GROK_AUTH_PATH` or `~/.grok/auth.json` | Grok Build CLI integration, separate from `grok` OAuth. |
| `lmstudio` | Configured `openai_compatible` endpoint and loaded model | A custom provider name, not a separate built-in adapter. |
| Other custom names | Explicit `type` and endpoint; `api_key` or `<PROVIDER_NAME>_API_KEY` when required | Unknown provider names default to `openai_compatible`; configure them before use. |

`debug` is a local test provider, not a hosted service. See the [compaction debugging example](/guides/debugging/#exercise-automatic-compaction-locally).

### OAuth sign-in and sign-out

```bash
term-llm auth login chatgpt
term-llm auth login grok
term-llm auth login copilot
term-llm auth status
term-llm auth logout chatgpt
```

Use the companion CLI's own sign-in process for `*-bin` integrations; `term-llm auth login` is not their login manager. Keep OAuth files private and do not copy tokens into project configuration. Copilot chat OAuth is also separate from the GitHub billing credentials used by [live usage reporting](/reference/usage-tracking/).

### OpenCode Zen

The shipped Zen main and fast defaults are `mimo-v2.5-free`. To select that model explicitly, including on an older installation or one with a saved model override:

```bash
term-llm ask --provider zen:mimo-v2.5-free "Explain git rebase in three sentences"
term-llm models --provider zen
```

Zen uses the OpenAI-compatible Chat Completions endpoint at `https://opencode.ai/zen/v1`; unlike OpenCode Go, this adapter does not dynamically switch to Responses or Anthropic Messages. Only models compatible with that endpoint can be used through it. Reasoning-effort suffixes are offered when the catalog advertises discrete efforts; a model having internal reasoning does not mean it accepts every effort suffix.

A model can remain listed while unavailable upstream. If a request reports an unsupported or unavailable model, choose another currently usable free model or a different configured provider. Do not assume `gpt-5-nano` or a model from an old free-model list is still free. An explicit saved `providers.zen.model` or `fast_model` takes precedence over new shipped defaults.

### Companion CLI providers

```bash
term-llm ask --provider claude-bin "Explain this function"
term-llm models --provider cursor-bin
term-llm models --provider agy-bin
term-llm models --provider grok-bin
```

Install and authenticate the corresponding CLI first. These are local subprocess integrations with their own version, account, and model constraints—not local inference. Prompts and tool context can still be sent to the companion service. `claude-bin` offers aliases such as `sonnet`, `opus`, `fable`, and `haiku`; the other integrations can discover models through their CLI.

term-llm supplies its agent instructions and tool/approval flow to these integrations. Do not assume every companion CLI feature or setting is inherited unchanged. Claude Code hooks are disabled by default; `providers.claude-bin.enable_hooks: true` is an explicit opt-in. For provider-specific environment overrides and setup, see [Provider setup details](/reference/provider-setup-details/#option-11-use-claude-code-claude-bin).

### OpenCode Go

```bash
export OPENCODE_API_KEY=your-key
term-llm models --provider opencode-go
term-llm ask --provider opencode-go:glm-5.2 "question"
```

OpenCode Go's model set changes frequently. term-llm merges the live Go `/models` availability list with OpenCode's model catalog for protocol, limits, pricing, and reasoning metadata, then caches the result for five minutes. Models marked for `@ai-sdk/openai-compatible`, `@ai-sdk/openai`, and `@ai-sdk/anthropic` are sent to the Go Chat Completions, Responses, and Messages endpoints respectively. Effort-based models expose their advertised suffixes, while budget-based Anthropic-compatible models such as `qwen3.8-max` expose `-high` and `-max` reasoning variants. A newly available model without catalog metadata normally falls back to Chat Completions; preview Muse Spark models have a Responses fallback. Protocol and model availability still depend on the upstream service.

A compatible gateway can be configured under a custom provider name. `base_url` replaces `https://opencode.ai/zen/go/v1` while preserving dynamic protocol routing. Use `base_url`, not a full-endpoint `url` override:

```yaml
providers:
  private-go:
    type: opencode-go
    base_url: https://gateway.example.com/v1
    api_key: ${PRIVATE_GO_API_KEY}
    model: muse-spark-1.2-contributor
```

Examples:

```bash
term-llm ask --provider anthropic "question"
term-llm ask --provider chatgpt "question"
term-llm ask --provider grok "question"
term-llm ask --provider copilot "question"
```

`grok` in chat selects the subscription OAuth provider. Existing `providers.grok` API-key or custom-endpoint configurations must declare `type: xai` or `type: openai_compatible`; term-llm rejects ambiguous `api_key`, `base_url`, and `url` fields instead of ignoring them. The image command keeps its older `--provider grok` alias for the xAI API-key image provider (`image.xai` / `XAI_API_KEY`), not Grok subscription OAuth.

### Anthropic-compatible endpoints

Use `type: anthropic` with `base_url` to point the native Anthropic protocol provider at a compatible gateway or self-hosted endpoint:

```yaml
providers:
  custom-anthropic:
    type: anthropic
    base_url: https://gateway.example.com/anthropic
    api_key: ${CUSTOM_ANTHROPIC_API_KEY}
    model: custom-model
```

The Anthropic SDK appends paths such as `/v1/messages` and `/v1/models` to `base_url`. The field supports the same lazy `srv://` and `$()` endpoint resolution as `url`. Omit `base_url` to use Anthropic's standard API endpoint.

## WebSocket defaults

The built-in `openai` text provider defaults to **HTTP/SSE** (`use_websocket: false`); `chatgpt` defaults to **Responses WebSockets** (`use_websocket: true`). OpenAI can opt in with `providers.openai.use_websocket: true`. The WebSocket path can reduce connection overhead in agentic/tool-heavy runs by reusing one connection and continuing compatible turns with `previous_response_id` plus only new input. If setup fails before streaming starts, term-llm falls back to HTTP/SSE; if a WebSocket continuation rejects the previous response ID, it retries once with full input.

To force HTTP/SSE for either built-in provider:

```yaml
providers:
  openai:
    use_websocket: false
  chatgpt:
    use_websocket: false
```

OpenAI-compatible providers remain HTTP/SSE by default. WebSocket defaults are not applied to `type: openai_compatible` entries.

## GPT-6 Astra on ChatGPT

`gpt-6-astra` is supported through the ChatGPT OAuth adapter when it is available in your authenticated catalog. It is distinct from the shipped ChatGPT default (`gpt-5.6-sol-medium`).

```bash
term-llm models --provider chatgpt
term-llm ask --provider chatgpt:gpt-6-astra "Explain this design"
term-llm ask --provider chatgpt:gpt-6-astra-fast "Explain this design"
```

The `-fast` suffix requests the fast/priority service tier; it does not select `fast_model` or a different model. Support depends on the model/account. Choose reasoning effort from the catalog's advertised values rather than assuming every GPT-family suffix is available. Astra's bundled context recommendation is 372K, with an 872K fallback maximum; the account's known maximum and explicit configuration govern the selected budget as described below.

Do not infer public OpenAI API support, pricing, or advanced Responses controls from an OAuth catalog entry. Those paths have separate capability gates.

## ChatGPT context policy

`term-llm models -p chatgpt` queries the authenticated Codex catalog, using a
five-minute cache and stale-cache/static fallbacks when offline. Hidden Codex
picker entries remain available for explicit model selection. ChatGPT `models`
configuration augments the discovered shell-completion catalog rather than
restricting it to the entries you customize.

term-llm chooses context in this order:

1. `models[].context_window` for the selected upstream model or alias.
2. Provider-level `context_window` (including newly discovered models).
3. term-llm's shipped input budget.
4. The backend default for an unknown model.

The selected context is capped at the account's known `max_context_window`.
The backend's `context_window` is a default, **not** that ceiling. When offline,
the last cached maximum is used; without cache, bundled maxima are used where
known. For models with no known maximum, an available backend default or shipped
budget is the conservative ceiling; a configuration value alone cannot establish
a larger supported window.

The shipped budgets for **GPT-6 Astra and GPT-5.6 Sol/Terra/Luna are 372,000
tokens**. GPT-5.4 retains its previous 922,000-token budget. These are capped when
the account reports a smaller maximum. Astra's bundled maximum is 872,000, not
its 272,000-token Codex default.

```yaml
providers:
  chatgpt:
    # Optional default for all ChatGPT models; each model's maximum still applies.
    context_window: 372000
    models:
      - id: gpt-6-astra
        context_window: 600000
      - id: gpt-5.6-sol
        context_window: 372000
```

Omit `context_window` (or set it to zero) to use the shipped policy. Context
settings control local tracking and automatic compaction; they are not sent as
an unsupported Responses API parameter. With `context_window` alone, the selected
window is the input budget. If `max_output_tokens` is explicitly configured too,
it is reserved **after** clamping the context. Shipped budgets already include
headroom and do not have the model's theoretical output maximum subtracted again.
The existing soft/hard compaction thresholds apply to the resulting input budget.
An output reservation that consumes the entire window leaves a minimum one-token
input budget; choose a smaller reservation instead.

Model listing shows **selected**, **recommended**, and **max** context.
Recommended is the shipped/default budget capped at the known maximum; selected
is the local context target after user overrides and clamping, not a server-confirmed
allocation. Only when an explicit output reservation reduces the input budget,
a second line shows **Input budget** and **Output reserve**. The reserve is local
budgeting, not an enforced ChatGPT generation limit. For example:

```text
gpt-6-astra [context: 600K selected, 372K recommended, 872K max]
  Input budget: 580K · Output reserve: 20K
```

`--json` continues to include
`backend_context`, `recommended_context`, `max_context`, `configured_context`,
and `input_limit`. Caches store backend facts, not user settings: changing config
does not require clearing the cache. Custom provider keys with `type: chatgpt`
have independent context settings.

## GPT-5.6 on OpenAI and ChatGPT

term-llm ships fallback metadata for the GPT-5.6 family:

| Provider | Default model | Fast model | Effective input | Max output | Efforts |
|---|---|---|---:|---:|---|
| `openai` (API key) | `gpt-5.6-sol` | `gpt-5.6-luna` | 922K | 128K | `none`, `low`, `medium`, `high`, `xhigh`, `max` |
| `chatgpt` (OAuth) | `gpt-5.6-sol-medium` | `gpt-5.6-luna` | 372K fallback | 128K | `low`, `medium`, `high`, `xhigh`, `max` |

The OpenAI model IDs are `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`. ChatGPT exposes the same named variants through its live Codex catalog; live input limits and reasoning metadata take precedence over the static fallback. Luna keeps its upstream medium default unless you explicitly select another effort.

**Pro and Ultra are not reasoning efforts.** On the public OpenAI API, Pro sends `reasoning.mode: pro`; Standard sends `reasoning.mode: standard`. In terminal chat use `/pro on`, `/pro off`, or `/pro status`. The browser shows a Standard/Pro selector only when the selected model advertises it. ChatGPT OAuth does not receive public-API Pro mode. Codex's separate product-level Ultra option runs at `max` effort and adds subagents, so term-llm does not expose it as an effort.

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

## Ollama

The built-in `ollama` provider uses Ollama's **native** `/api/chat` API, not its `/v1/chat/completions` compatibility endpoint. It discovers installed models through `/api/tags` and supports streaming, model-supported tool calls, and Ollama-native generation options.

Start Ollama first (`ollama serve` if it is not already running), then pull a model and try it:

```bash
ollama pull qwen2.5-coder:7b
term-llm models --provider ollama
term-llm ask --provider ollama:qwen2.5-coder:7b "Explain git rebase"
```

The shipped main and fast model defaults are `qwen2.5-coder:7b`. term-llm does not download models automatically. Select an installed model suitable for your hardware and task; a text response succeeding does not establish reliable agent tool use.

```yaml
providers:
  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
    model: qwen2.5-coder:7b
    fast_model: qwen2.5-coder:7b
    # Optional server-side generation controls:
    num_ctx: 32768
    num_predict: 4096
```

Without an explicit `base_url`, the adapter checks `OLLAMA_HOST`, then uses `http://127.0.0.1:11434`. A host without a URL scheme is interpreted as HTTP. Do **not** append `/v1` to a native Ollama base URL. For an intentionally OpenAI-compatible Ollama connection, use a separate profile such as `ollama_compat` with `type: openai_compatible` and `base_url: http://127.0.0.1:11434/v1`.

| Ollama field | Purpose |
|---|---|
| `think` | Boolean thinking switch, for models that support it. |
| `think_level` | Named thinking level; takes precedence over `think`. `xhigh` normalizes to `max`; supported levels depend on the model/server. |
| `num_ctx` | Context size requested from the Ollama runtime. |
| `num_predict` | Maximum generated tokens requested from Ollama. |
| `top_k`, `min_p`, `presence_penalty` | Native sampling options; set only when appropriate for the model. |

`context_window` is term-llm's local context/compaction metadata; it does not by itself configure Ollama's server-side `num_ctx`. Keep those settings consistent with the model and available memory. A local chat backend also does not make external search, MCP services, or a separately configured Guardian provider local.

## LM Studio

Start LM Studio's local API server and load a model. Configure a profile before asking term-llm to discover or use it:

```yaml
providers:
  lmstudio:
    type: openai_compatible
    base_url: http://127.0.0.1:1234/v1
    model: your-loaded-model
```

```bash
term-llm models --provider lmstudio
term-llm ask --provider lmstudio:MODEL_ID "Explain git rebase"
```

Replace `MODEL_ID` and `your-loaded-model` with the identifier exposed by your LM Studio server. If you enabled authentication on the server, configure its API key too. This profile uses the generic OpenAI-compatible adapter; Ollama-specific fields such as `think_level` and `num_ctx` do not apply.

## Ollama thinking levels

Ollama's native chat API accepts either a boolean `think` switch or a named thinking level. Use `think` for binary on/off models, or `think_level` when the model supports `low`, `medium`, `high`, or `max`:

```yaml
providers:
  local_oss:
    type: ollama
    base_url: http://127.0.0.1:11434
    model: gpt-oss:20b
    think_level: low
```

`think_level` takes precedence when both settings are present. Named levels are useful when binary thinking produces unnecessarily long traces; support still depends on the selected Ollama model.

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

For vLLM templates that use `chat_template_kwargs.thinking`, declare the model's exact capabilities and default in config. `vllm_thinking_param: thinking` selects the request shape; term-llm does not embed model-specific effort mappings:

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

| Configured effort | Request fields sent to vLLM |
|---|---|
| `none` | `chat_template_kwargs.thinking: false`; omit top-level `reasoning_effort` |
| any enabled effort | `chat_template_kwargs.thinking: true`; pass the same value as top-level `reasoning_effort` |

The bare provider/model uses `default_reasoning_effort`. `-p cdck_deepseek-<TAB>` completes only the values listed in `reasoning_efforts`. Model-name detection for `deepseek` remains as a backward-compatible fallback when `vllm_thinking_param` is omitted; explicit configuration is preferred for aliases. Requests using the `thinking` shape do not send `thinking_token_budget`.

Notes:

- Start vLLM with the appropriate reasoning parser for your model, for example `--reasoning-parser qwen3` for Qwen3-family reasoning output or `--reasoning-parser deepseek_v3` / `deepseek_v4` for DeepSeek variants.
- Qwen `thinking_token_budget` requires a vLLM server new enough to support it and, on recent vLLM, a server-side `--reasoning-config`. Plain/default Qwen requests omit the budget so they work without that extra server option.
- vLLM currently streams reasoning text in `delta.reasoning`, but may not report `usage.completion_tokens_details.reasoning_tokens` accurately. In that case term-llm can show reasoning in debug output while `reasoning_tokens` remains `0`; this reflects vLLM usage metadata, not missing reasoning text.
- For multi-turn conversations, term-llm persists streamed reasoning and replays it as assistant `reasoning` on the next vLLM request. This lets vLLM's chat template render the prior reasoning consistently and gives prefix caching the best chance to reuse shared prompt prefixes when the server has prefix caching enabled.

## OpenAI-compatible providers

For local or custom backends that do not need vLLM chat-template thinking controls, use `type: openai_compatible`.

```yaml
providers:
  ollama_compat:
    type: openai_compatible # optional alternative to the native ollama adapter
    base_url: http://127.0.0.1:11434/v1
    model: qwen2.5-coder:7b

  lmstudio:
    type: openai_compatible
    base_url: http://localhost:1234/v1
    model: your-loaded-model

  cerebras:
    type: openai_compatible
    base_url: https://api.cerebras.ai/v1
    model: your-cerebras-model
    api_key: ${CEREBRAS_API_KEY}
```

Use `base_url` when the standard `/chat/completions` path should be appended automatically. Use `url` when you need to specify the full chat completions endpoint directly.

### Configuration reference

| Field | Type | Description |
|---|---|---|
| `type` | string | Use `openai_compatible` for generic custom providers, or `vllm` for vLLM servers that should receive reasoning controls for Qwen/DeepSeek-style chat templates. An explicit type wins; `ollama` infers native `ollama`, `vllm` infers `vllm`, and an unknown/custom name infers `openai_compatible`. |
| `base_url` | string | Base URL (e.g., `http://localhost:11434/v1`). `/chat/completions` is appended automatically. Supports `srv://` and `$()` resolution. |
| `url` | string | Full chat completions URL, used as-is. Use this when your endpoint path differs from the standard. Supports `srv://` for DNS SRV discovery and `$()` for command-based resolution. |
| `api_key` | string | API key. Supports `${ENV_VAR}`, `op://`, `file://`, and `$()` resolution. If omitted, term-llm tries `<PROVIDER_NAME>_API_KEY` from the environment. |
| `model` | string | Default model name. For configured model objects, this may be either the upstream `id` or the friendly `alias`. |
| `models` | list | Optional list for model pickers and shell completion. Entries may be strings or objects with `id`, optional `alias`, `context_window`, `max_output_tokens`, `parse_reasoning`, `include_reasoning`, `thinking_param`, `reasoning_efforts`, and `default_reasoning_effort`. |
| `fast_model` | string | Lightweight model used for control-plane tasks (e.g., title generation) and the agent `model: fast` alias. This is separate from service-tier fast mode. Usually this is all you need. |
| `fast_provider` | string | Optional provider key to use when the `fast_model` should run on a different configured provider than this one. |
| `service_tier` | string | Optional Responses API service tier for built-in `openai` and `chatgpt` providers. Use `fast` or `priority` to request fast/priority service where the selected model supports it. Omit the field to send no service tier. |
| `context_window` | int | Override context window size in tokens. For ChatGPT, overrides the shipped context target, capped at the known backend maximum. Also supports self-hosted models not in the built-in token limit tables. |
| `max_output_tokens` | int | Override maximum output tokens. Same use case as `context_window`. |
| `no_stream_options` | bool | When `true`, don't send `stream_options` in the request. Use this for servers that reject the field. Default `false`; compatible servers may use it to include usage in streamed responses. Native Ollama uses its own protocol and does not need this flag. |
| `parse_reasoning` | bool | Send `parse_reasoning` for OpenAI-compatible APIs that can parse inline model thinking into `reasoning_content` (for example Friendli). |
| `include_reasoning` | bool | Send `include_reasoning`; useful with `parse_reasoning: true` when you want streamed `delta.reasoning_content` events. |
| `thinking_param` | string | Generic OpenAI-compatible chat-template control. When a reasoning effort is selected (for example a `-high`/`-max` suffix), term-llm sends `chat_template_kwargs.<thinking_param>: true`. Friendli GLM-5.2 uses `enable_thinking`. |
| `vllm_thinking_param` | string | `type: vllm` only. Override the chat-template thinking key when auto-detection is not possible: `enable_thinking` for Qwen-style templates, `thinking` for DeepSeek-style templates. |
| `use_websocket` | bool | Reserved for providers with native Responses WebSocket support. Defaults to `true` for `chatgpt` and `false` for `openai`. Generic OpenAI-compatible providers use HTTP/SSE, not this native transport. |

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
        reasoning_efforts: [none, low, high, max]
        default_reasoning_effort: high
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
| `thinking_param` | Per-model override for the provider-level `thinking_param` key. Sent as `chat_template_kwargs.<thinking_param>: true` when the effective effort is non-empty. |
| `reasoning_efforts` | Exact suffixes to expose for this model, for example `[none, low, high, max]`. They drive model aliases, effort selectors, provider-profile completion, and configured provider-profile validation. |
| `default_reasoning_effort` | Effort used by the bare model/provider when no request or model suffix overrides it. Must be listed in `reasoning_efforts`. |
| `vision_via` | Optional `provider` or `provider:model` route for indirect image understanding. Can be set at provider level (`providers.<name>.vision_via`) as the default for all models on that provider, or on an individual model object to override the provider default. If only `provider` is given, that provider's configured default `model` is used. When set, uploaded image parts are replaced with local path references for the primary model, `view_image` is auto-enabled, and that tool asks the configured vision model to return a text-only analysis. Requires the primary model to support tool calls and the vision provider credentials to be configured. |

For `-p custom:friendly-name-max`, term-llm sends `model: upstream/model-id` plus `reasoning_effort: max`. When `default_reasoning_effort` is set, the bare provider/model uses that effort; otherwise it sends no effort. If `reasoning_efforts` is empty or omitted, no configured effort aliases or provider-profile completions are generated for that model.

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

For ChatGPT OAuth, GPT-5.6 Sol, Terra, and Luna expose `low`, `medium`, `high`, `xhigh`, and `max`. Codex's product-level Ultra option is not an inference effort: the official client sends `max` and separately enables subagents, so it is not included in term-llm's effort suffixes.

### vLLM thinking suffixes

For configured providers with `type: vllm`, effort suffixes can be applied directly to the provider name. Declare them on the selected model to drive parsing and shell completion:

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

```bash
term-llm ask -p cdck_deepseek "uses configured high default"
term-llm ask -p cdck_deepseek-low "override with low"
term-llm ask -p cdck_deepseek-none "disable thinking"
```

The suffix is stripped before the model ID is sent upstream. `-p cdck_deepseek-<TAB>` offers exactly the declared efforts, and undeclared provider-profile suffixes are rejected.

For Qwen-style `enable_thinking` templates, term-llm retains its budget mapping:

| Suffix | Qwen/vLLM behavior |
|---|---|
| `none` | `enable_thinking: false`, no `thinking_token_budget` |
| `-low` | enable thinking, budget `1024` |
| `-medium` | enable thinking, budget `4096` |
| `-high` / `-xhigh` / `-max` | enable thinking, budget `10000` |

For `thinking` templates, configured enabled efforts pass through unchanged:

| Effort | vLLM behavior |
|---|---|
| `none` | `thinking: false`; omit top-level `reasoning_effort` |
| any configured enabled value | `thinking: true`; send that value as top-level `reasoning_effort` |

Reasoning replay uses vLLM's `reasoning` assistant-message field. vLLM may still report `reasoning_tokens: 0` in usage metadata even when reasoning text was streamed; this is a known vLLM-side accounting gap.

### Anthropic extended thinking

For supported Anthropic models, append `-thinking`:

```bash
term-llm ask --provider anthropic:claude-sonnet-4-6-thinking "complex question"
```

```yaml
providers:
  anthropic:
    model: claude-sonnet-4-6-thinking
```

On the direct Anthropic adapter, `-thinking` uses adaptive thinking for recognized Claude 4.6+ Opus/Sonnet and Fable models; older supported models use a token budget. Effort suffixes are separate: Opus/Fable expose `-low`, `-medium`, `-high`, `-xhigh`, and `-max`, while Sonnet exposes `-low`, `-medium`, and `-high`. Availability remains model-specific; use only controls accepted by your endpoint.

### AWS Bedrock

The `bedrock` provider routes Anthropic Claude models through AWS Bedrock. It shares the Anthropic message/tool implementation and translates `-thinking` and `-1m` suffixes. Model availability, context features, and hosted tools remain subject to AWS support and your account; this is not a guarantee of feature parity with the direct Anthropic service. The Bedrock constructor does not currently translate the direct adapter’s `-high`/`-max` effort suffixes—do not append those to a Bedrock model ID.

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

The geographic prefix (`us.`, `eu.`, `ap.`) is derived from the configured region automatically. For example, `eu-west-1` produces `eu.anthropic.*` IDs, `ap-southeast-1` produces `ap.anthropic.*`, etc. These are cross-region inference profiles, not a guarantee that data stays in the single configured region. The current mapper uses `eu` for `eu-*`, `ap` for `ap-*`, and `us` otherwise. For residency-sensitive workloads, explicitly choose an AWS-supported model or profile in `model_map` and verify its routing policy.

Raw Bedrock model IDs (`us.anthropic.claude-sonnet-4-6`, `anthropic.claude-sonnet-4-6`) and full ARNs are passed through without translation.

| Config field | Description |
|---|---|
| `region` | AWS region. Uses the AWS SDK region configuration (including `AWS_REGION` / `AWS_DEFAULT_REGION`), then `us-east-1` if unresolved. |
| `profile` | AWS profile name from `~/.aws/credentials`. |
| `access_key_id` | Explicit AWS access key. Supports `$()`, `op://`, `${ENV}`. |
| `secret_access_key` | Explicit AWS secret key. Same resolution support. |
| `session_token` | Optional session token for temporary credentials. |
| `model_map` | Map of friendly names to Bedrock model IDs or ARNs. |

## Native search support

Some providers support native web search. Others rely on external search tooling.

The implemented adapters expose these native search routes:

| Provider | Native route | Important distinction |
|---|---|---|
| `anthropic` | Anthropic web search and web fetch tools | Model and endpoint must accept the hosted tools. |
| `openai`, `chatgpt` | Responses web search | URL reading can still use an external fetch tool. |
| `xai` | Web and X search | Native search does not combine with ordinary client tool definitions in the same request. |
| `gemini` | Google Search grounding | Thinking configuration is suppressed when native search or function tools are active in this adapter. |
| `venice` | Venice web search | Independent of the external `search.provider`. |
| `grok-bin` | Companion CLI search/fetch | Not the same as the `grok` subscription OAuth adapter. |
| `bedrock` | Advertises the shared Anthropic native-tool path | Do not assume AWS accepts every Anthropic hosted tool/beta. Use external search if the endpoint rejects it. |

`grok` subscription OAuth, `claude-bin`, `cursor-bin`, `agy-bin`, `copilot`, `zen`, `opencode-go`, `openrouter`, and local adapters use term-llm's external search tools rather than claiming those native capabilities. Tool support by the selected model is still required.

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
- **local inference:** native `ollama`, LM Studio, or another configured local endpoint
- **subscription-backed access:** `chatgpt`, `grok`, `copilot`, or `opencode-go`, according to your account
- **reuse a companion CLI:** `claude-bin`, `cursor-bin`, `agy-bin`, or `grok-bin`

## Related pages

- [Configuration](/reference/configuration/)
- [Search](/guides/search/)
- [Providers and setup](/getting-started/providers-and-setup/)
