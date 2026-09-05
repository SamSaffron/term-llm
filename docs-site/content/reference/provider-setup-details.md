---
title: "Provider setup details"
weight: 4
description: "Provider-specific authentication, configuration examples, local endpoints, and subscription setup."
kicker: "Providers"
source_readme_heading: "Setup"
featured: true
next:
  label: Usage guide
  url: /guides/usage/
---
Start with [Providers and setup](/getting-started/providers-and-setup/) if you just want a working first connection. This page contains the detailed settings for each provider.

On an interactive first run, term-llm helps you choose a provider. See the [provider inventory](/reference/providers-and-models/#credentials) for the complete set of built-in adapters, companion CLIs, and custom endpoint types.

### Option 1: Try it free with Zen

[OpenCode Zen](https://opencode.ai) provides free access to multiple models. No API key required:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
term-llm ask --provider zen:mimo-v2.5-free "quick question"  # explicit shipped free default
```

The shipped main and fast defaults are `mimo-v2.5-free`. Free model availability and limits are controlled by OpenCode Zen and can change; a listed model may still be unavailable. Discover the current catalog with `term-llm models --provider zen`; paid models require a Zen API key.

Or configure as default:

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: zen
providers:
  zen:
    model: mimo-v2.5-free
    fast_model: mimo-v2.5-free
```

### Option 2: Use API key

Set your API key as an environment variable:

```bash
# For Anthropic
export ANTHROPIC_API_KEY=your-key

# For OpenAI
export OPENAI_API_KEY=your-key

# For xAI (Grok)
export XAI_API_KEY=your-key

# For Venice
export VENICE_API_KEY=your-key

# For NEAR AI Cloud
export NEARAI_API_KEY=your-key

# For SambaNova Cloud
export SAMBANOVA_API_KEY=your-key

# For OpenRouter
export OPENROUTER_API_KEY=your-key

# For Gemini
export GEMINI_API_KEY=your-key

# Optional: for higher Exa MCP search limits, or direct Exa API search
export EXA_API_KEY=your-key

# For Perplexity search (used by search.provider: perplexity)
export PERPLEXITY_API_KEY=your-key

# For Parallel search (used by search.provider: parallel)
export PARALLEL_API_KEY=your-key
```

OpenAI API-key installs default to `gpt-5.6-sol`, with `gpt-5.6-luna` as the lightweight control-plane model:

```yaml
default_provider: openai
providers:
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
```

GPT-5.6 has a 922K effective input budget and a 128K output cap through the OpenAI provider. Its API efforts are `none`, `low`, `medium`, `high`, `xhigh`, and `max`. Public-API Pro mode and the advanced Responses controls are documented under [Providers and models](/reference/providers-and-models/#gpt-56-on-openai-and-chatgpt).

### Option 3: Use ChatGPT (Plus/Pro subscription)

If you have a ChatGPT Plus or Pro subscription, you can use the `chatgpt` provider with native OAuth authentication for both text and image workflows:

```bash
term-llm ask --provider chatgpt "explain this code"
term-llm ask --provider chatgpt:gpt-5.6-sol-max "hard code question"
term-llm ask --provider chatgpt:gpt-5.6-luna-medium "quick code question"
term-llm image --provider chatgpt:gpt-5.4 "storybook fox in the snow"
```

On first use, you'll be prompted to authenticate via browser. Credentials are stored locally and refreshed automatically.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: chatgpt

providers:
  chatgpt:
    model: gpt-5.6-sol-medium
    fast_model: gpt-5.6-luna
    # Request fast/priority service tier for supported ChatGPT models/accounts.
    # Omit this field to send no service_tier.
    service_tier: fast
    # Enabled by default for ChatGPT text requests; set false to force HTTP/SSE.
    use_websocket: true
```

The ChatGPT default pins Sol to medium effort. Its live model catalog supplies current limits and supported efforts; the static GPT-5.6 fallback is 372K effective input and 128K output. Sol, Terra, and Luna support `low`, `medium`, `high`, `xhigh`, and `max`, with Luna keeping its upstream medium default.

Codex's product-level **Ultra** option combines `max` effort with subagents; it is not an inference API effort and term-llm does not show it in the effort selector. The ChatGPT OAuth backend also does not receive the public API's Pro, hosted multi-agent, programmatic-tool-calling, or prompt-cache control fields. `service_tier: fast` is a user-facing alias for the upstream `priority` tier; omit it to send no tier.

### Grok subscription OAuth

If you have an eligible Grok subscription, use the native `grok` provider. It uses xAI's device OAuth flow and the Grok CLI Responses proxy; it does not use `XAI_API_KEY` or read the Grok Build CLI's `~/.grok/auth.json`.

```bash
term-llm auth login grok
term-llm models --provider grok
term-llm ask --provider grok:grok-4.6 "review this code"
```

Credentials are stored separately at `$XDG_CONFIG_HOME/term-llm/grok_oauth.json` (normally `~/.config/term-llm/grok_oauth.json`) with owner-only permissions and refreshed automatically. HTTP/SSE is always used with `store: false`; server-side response chaining and WebSockets are disabled.

```yaml
default_provider: grok
providers:
  grok:
    model: grok-4.6
    fast_model: grok-4.6
```

The authenticated subscription catalog determines which models the account can use. `xai` remains the separate API-key provider for the public developer API, and `grok-bin` remains the local Grok Build subprocess provider. Because `grok` is now the chat OAuth provider, an older `providers.grok` block containing `api_key`, `base_url`, or `url` must be migrated explicitly: use `type: xai` for the public xAI API, or `type: openai_compatible` for a custom endpoint. term-llm returns a migration error rather than silently ignoring those fields.

The overload is command-specific: `term-llm ask --provider grok` means Grok subscription OAuth, while `term-llm image --provider grok` remains a compatibility alias for the xAI image API and uses `image.xai.api_key` / `XAI_API_KEY`. Grok subscription OAuth credentials are not used for image generation.

### Option 4: Use xAI public developer API (Grok)

[xAI](https://x.ai) provides access to Grok models with native web search and X (Twitter) search capabilities.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: xai

providers:
  xai:
    model: grok-4-1-fast  # default model
```

**Available models:**
| Model | Context | Description |
|-------|---------|-------------|
| `grok-4-1-fast` | 2M | Latest, best for tool calling (default) |
| `grok-4-1-fast-reasoning` | 2M | With chain-of-thought reasoning |
| `grok-4-1-fast-non-reasoning` | 2M | Faster, no reasoning overhead |
| `grok-4` | 256K | Base Grok 4 model |
| `grok-3` / `grok-3-fast` | 131K | Previous generation |
| `grok-3-mini` / `grok-3-mini-fast` | 131K | Smaller, faster |
| `grok-code-fast-1` | 256K | Optimized for coding tasks |

Or use the `--provider` flag:

```bash
term-llm ask --provider xai "explain quantum computing"
term-llm ask --provider xai -s "latest xAI news"  # uses native web + X search
term-llm ask --provider xai:grok-4-1-fast-reasoning "solve this step by step"
term-llm ask --provider xai:grok-code-fast-1 "review this code"
```

### Option 5: Use Venice

[Venice](https://venice.ai) exposes a wide mix of hosted text models behind an OpenAI-compatible API, including Venice's own uncensored models plus Claude, Gemini, Grok, Qwen, GLM, Kimi, DeepSeek, and more. term-llm also enables Venice native web search when you use `-s` / `--search`.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: venice

providers:
  venice:
    model: venice-uncensored  # default model
    fast_model: llama-3.2-3b  # lightweight control-plane model
```

Or use the `--provider` flag directly:

```bash
term-llm ask --provider venice "explain quantum computing"
term-llm ask --provider venice:grok-4-20-beta -s "latest xAI news"
term-llm ask --provider venice:qwen3-coder-480b-a35b-instruct "review this code"
term-llm models --provider venice
```

### Option 6: Use NEAR AI Cloud

[NEAR AI Cloud](https://cloud.near.ai) provides OpenAI-compatible TEE-backed private inference. Set `NEARAI_API_KEY` or put `api_key` under `providers.nearai`.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: nearai

providers:
  nearai:
    model: zai-org/GLM-5.1-FP8
    fast_model: Qwen/Qwen3.6-35B-A3B-FP8
```

```bash
term-llm ask --provider nearai "explain trusted execution environments"
term-llm ask --provider nearai:Qwen/Qwen3.6-35B-A3B-FP8 "summarize this repository"
term-llm models --provider nearai
```

`term-llm models --provider nearai` queries NEAR AI Cloud's public model catalog and shows chat-capable models with context windows and token prices when available.

### Option 7: Use SambaNova Cloud

[SambaNova Cloud](https://cloud.sambanova.ai/) provides fast OpenAI-compatible hosted inference on RDU hardware. Set `SAMBANOVA_API_KEY` or put `api_key` under `providers.sambanova`.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: sambanova

providers:
  sambanova:
    model: gpt-oss-120b
    fast_model: Meta-Llama-3.3-70B-Instruct
```

```bash
term-llm ask --provider sambanova "explain RDUs"
term-llm ask --provider sambanova:MiniMax-M2.7 "summarize this repository"
term-llm models --provider sambanova
```

`term-llm models --provider sambanova` shows known SambaNova token prices when available. The bundled pricing table is synced from SambaNova's public pricing page.

### Option 8: Use OpenRouter

[OpenRouter](https://openrouter.ai) provides a unified OpenAI-compatible API across many models. term-llm sends attribution headers by default.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: openrouter

providers:
  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm
```

### Model Discovery

List available models from any supported provider:

```bash
term-llm models --provider anthropic  # List Anthropic models
term-llm models --provider openrouter # List OpenRouter models
term-llm models --provider nearai     # List NEAR AI Cloud models
term-llm models --provider sambanova  # List SambaNova models
term-llm models --provider ollama     # List local Ollama models
term-llm models --provider lmstudio   # List local LM Studio models
term-llm models --json                # Output as JSON
```

### Provider Discovery

List all available LLM providers and their configuration status:

```bash
term-llm providers                 # List all providers
term-llm providers --configured    # Only show configured providers
term-llm providers --builtin       # Only show built-in providers
term-llm providers anthropic       # Show details for specific provider
term-llm providers --json          # JSON output
```

### Option 9: Use AWS Bedrock

[AWS Bedrock](https://aws.amazon.com/bedrock/) provides access to Anthropic Claude models through your AWS account. This is useful for organizations that route AI usage through AWS billing, need VPC/PrivateLink access, or use application inference profiles for rate/cost management.

```bash
term-llm ask --provider bedrock:claude-sonnet-4-6 "explain this code"
term-llm ask --provider bedrock:claude-opus-4-6-thinking "complex question"
```

**Authentication** uses the standard AWS credential chain. Configure credentials via any method the AWS SDK supports:

```bash
# Environment variables
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-west-2
```

Or configure fully in `~/.config/term-llm/config.yaml`:

```yaml
default_provider: bedrock

providers:
  bedrock:
    region: us-west-2
    model: claude-sonnet-4-6-thinking
```

**Explicit credentials** (with 1Password, vaults, or `$()` command resolution):

```yaml
providers:
  bedrock:
    region: us-west-2
    access_key_id: $(op-cache read "op://Private/AWS Bedrock/AWS_ACCESS_KEY_ID")
    secret_access_key: $(op-cache read "op://Private/AWS Bedrock/AWS_SECRET_ACCESS_KEY")
    model: claude-sonnet-4-6-thinking
```

**Application inference profiles**: use `model_map` to alias friendly model names to Bedrock ARNs or model IDs. The `-thinking` and `-1m` suffixes work with mapped names:

```yaml
providers:
  bedrock:
    region: us-west-2
    access_key_id: $(op-cache read "op://Private/AWS Bedrock/AWS_ACCESS_KEY_ID")
    secret_access_key: $(op-cache read "op://Private/AWS Bedrock/AWS_SECRET_ACCESS_KEY")
    model: claude-sonnet-4-6-thinking
    model_map:
      claude-sonnet-4-6: arn:aws:bedrock:us-west-2:123456789:application-inference-profile/abc123
      claude-opus-4-6: arn:aws:bedrock:us-west-2:123456789:application-inference-profile/def456
      claude-haiku-4-5: arn:aws:bedrock:us-west-2:123456789:application-inference-profile/ghi789
```

With this config, `--provider bedrock:claude-opus-4-6-1m-thinking` strips the suffixes, resolves `claude-opus-4-6` through `model_map` to the ARN, and enables adaptive thinking + 1M context.

**Examples of built-in friendly-name translations** (AWS availability and access are account/region-specific):

| Model | Suffixes | Description |
|-------|----------|-------------|
| `claude-sonnet-4-6` | `-thinking`, `-1m` | Sonnet example |
| `claude-opus-4-7` | `-thinking`, `-1m` | Opus example |
| `claude-opus-4-6` | `-thinking`, `-1m` | Earlier Opus example |
| `claude-haiku-4-5` | `-thinking` | Fast, lightweight |

The geographic prefix (`us.`, `eu.`, `ap.`) is derived from your configured region. For example, `eu-west-1` produces `eu.anthropic.*` IDs, etc. These are cross-region profiles, not a single-region residency guarantee. The mapper uses `eu` for `eu-*`, `ap` for `ap-*`, and `us` for other region names. Explicitly choose and verify an AWS model/inference profile when routing restrictions matter.

You can also pass raw Bedrock model IDs directly (e.g., `us.anthropic.claude-sonnet-4-6` or full ARNs). These bypass the translation layer.

**Capabilities:** the adapter shares Anthropic message streaming, tools, images, thinking, and caching logic. AWS model, context, and hosted-tool support still apply; not every direct Anthropic feature is guaranteed on Bedrock. Use `--no-native-search` when the endpoint does not accept the native search/fetch tools. Bedrock translates `-thinking` and `-1m`, not the direct adapter’s effort suffixes such as `-high` and `-max`.

| Config field | Description |
|---|---|
| `region` | AWS region. Falls back to `AWS_REGION` / `AWS_DEFAULT_REGION` env vars, then `us-east-1`. |
| `profile` | AWS profile name from `~/.aws/credentials`. |
| `access_key_id` | Explicit AWS access key. Supports `$()`, `op://`, `${ENV}` resolution. |
| `secret_access_key` | Explicit AWS secret key. Same resolution support. |
| `session_token` | Optional session token for temporary/assumed-role credentials. |
| `model_map` | Map of friendly names to Bedrock model IDs or ARNs. |
| `model` | Default model (friendly name, Bedrock ID, or ARN). |

### Option 10: Use local LLMs (Ollama, LM Studio)

Run models locally with [Ollama](https://ollama.com) or [LM Studio](https://lmstudio.ai). Start the server and load/download a model first. Ollama is a native adapter; LM Studio needs an OpenAI-compatible profile. Configure the profiles below before listing models, and substitute your installed/loaded model IDs:

```bash
# List available models from your local server
term-llm models --provider ollama
term-llm models --provider lmstudio

# Configure in ~/.config/term-llm/config.yaml
```

```yaml
default_provider: ollama

providers:
  ollama:
    type: ollama
    base_url: http://127.0.0.1:11434
    model: qwen2.5-coder:7b
    fast_model: qwen2.5-coder:7b

  lmstudio:
    type: openai_compatible
    base_url: http://127.0.0.1:1234/v1
    model: your-loaded-model
```

For vLLM servers hosting Qwen or DeepSeek reasoning models, use `type: vllm` to enable vLLM chat-template thinking controls:

```yaml
providers:
  my-qwen:
    type: vllm
    base_url: http://gpu-server:8000/v1
    model: Qwen/Qwen3.5-122B-A10B
    context_window: 200000
    max_output_tokens: 50000
```

Then choose thinking effort with the provider suffix. Qwen uses `enable_thinking` plus token budgets for budgeted efforts:

```bash
term-llm ask -p my-qwen "hello"       # default: no thinking, no thinking_token_budget
term-llm ask -p my-qwen-low "hard"    # Qwen budget 1024
term-llm ask -p my-qwen-high "harder" # Qwen budget 10000
```

DeepSeek-on-vLLM can use `chat_template_kwargs.thinking` plus a top-level `reasoning_effort`. Declare the exact supported efforts rather than relying on a hard-coded low-to-high mapping:

```yaml
providers:
  my-deepseek:
    type: vllm
    base_url: http://gpu-server:8000/v1
    model: deepseek-ai/DeepSeek-V4-Flash
    vllm_thinking_param: thinking
    models:
      - id: deepseek-ai/DeepSeek-V4-Flash
        reasoning_efforts: [none, low, high, max]
        default_reasoning_effort: none
```

```bash
term-llm ask -p my-deepseek "hello"       # configured default: thinking=false
term-llm ask -p my-deepseek-low "hard"    # thinking=true, reasoning_effort=low
term-llm ask -p my-deepseek-max "hardest" # thinking=true, reasoning_effort=max
```

Use the effort set accepted by your deployed model/template. See [vLLM reasoning providers](/reference/providers-and-models/#vllm-reasoning-providers) for the request fields and parsing behavior.

For other OpenAI-compatible servers (text-generation-inference, generic vLLM models without these chat-template thinking controls, etc.):

```yaml
providers:
  my-server:
    type: openai_compatible
    base_url: http://your-server:8080/v1
    model: mixtral-8x7b
    models:  # optional: list models for shell autocomplete
      - mixtral-8x7b
      - llama-3-70b
```

The `models` list enables tab completion for `--provider my-server:<TAB>`. The configured `model` is always included in completions.

The built-in `chatgpt` text provider enables Responses WebSockets by default. `openai` defaults to HTTP/SSE and can opt in with `use_websocket: true`. `openai_compatible` and `vllm` providers do not: local/self-hosted compatible APIs stay on HTTP/SSE by default.

If your server rejects `stream_options` (causing errors on connect), disable it:

```yaml
providers:
  my-server:
    type: openai_compatible
    base_url: http://your-server:8080/v1
    model: my-model
    no_stream_options: true
```

For custom models not in the built-in token limit tables, set `context_window` and `max_output_tokens` explicitly:

```yaml
providers:
  my-server:
    type: openai_compatible
    base_url: http://your-server:8080/v1
    model: my-model
    context_window: 32768
    max_output_tokens: 8192
```

See [Providers and models](/reference/providers-and-models/#configuration-reference) for the full list of OpenAI-compatible and vLLM provider options.

### Option 11: Use Claude Code (claude-bin)

If you have [Claude Code](https://claude.ai/code) installed and logged in, you can use the `claude-bin` provider to run completions through the local `claude` subprocess. This requires no API key - it uses Claude Code's existing authentication.

```bash
# Use directly via --provider flag (no config needed)
term-llm ask --provider claude-bin "explain this code"
term-llm ask --provider claude-bin:haiku "quick question"  # use haiku model
term-llm exec --provider claude-bin "list files"           # command suggestions
term-llm ask --provider claude-bin -s "latest news"        # with web search

# Or configure as default
```

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: claude-bin

providers:
  claude-bin:
    model: sonnet  # opus, fable, sonnet, or haiku
    env:
      IS_SANDBOX: "1"  # useful in trusted/sandboxed containers
      # Generate a long-lived token with: claude setup-token
      # Useful in CI or headless environments where interactive login isn't possible
      CLAUDE_CODE_OAUTH_TOKEN: "your-oauth-token-here"
    # Optional: Claude Code hooks are disabled by default; set to true to opt back in
    # enable_hooks: true
```

**Features:**
- No API key required - uses Claude Code's existing authentication
- term-llm tool access through its MCP bridge, subject to the configured tools and approvals
- Model selection: `opus`, `fable`, `sonnet` (default), `haiku`; model/account availability still applies
- Claude Code hooks are disabled by default to keep user hook automation out of term-llm inference sessions
- Optional `providers.claude-bin.enable_hooks: true` to opt back into Claude Code hooks
- Optional `providers.claude-bin.env` passthrough for Claude subprocess settings (for example `IS_SANDBOX=1` in trusted root-run containers)
- `providers.<name>.env` values support the same deferred resolution as other config values, including `file://...#json.path`, `op://...`, and `$()`
- Works immediately if Claude Code is installed and logged in

OpenAI-compatible providers support two URL options:
- `base_url`: Base URL (e.g., `https://api.cerebras.ai/v1`) - `/chat/completions` is appended automatically
- `url`: Full URL (e.g., `https://api.cerebras.ai/v1/chat/completions`) - used as-is without appending

Use `url` when your endpoint doesn't follow the standard `/chat/completions` path, or to paste URLs directly from API documentation.

### Other companion CLIs

`cursor-bin`, `agy-bin`, and `grok-bin` reuse an installed, authenticated `cursor-agent`, `agy`, or `grok` binary respectively. They are distinct from hosted API-key integrations and from term-llm-managed OAuth.

```bash
term-llm models --provider cursor-bin
term-llm models --provider agy-bin
term-llm models --provider grok-bin
term-llm ask --provider cursor-bin "Explain this code"
```

Authenticate with the companion CLI first. See the [provider inventory](/reference/providers-and-models/#credentials) for credential sources and the [CLI wire debugging guide](/guides/debugging/#audit-cli-provider-wire-traffic) for diagnosing subprocess behavior. A locally installed CLI still sends inference to its service; it is not local model hosting.

### Option 12: Use GitHub Copilot

The `copilot` provider uses GitHub device-flow OAuth. Sign in and inspect the models available to your account:

```bash
term-llm auth login copilot
term-llm models --provider copilot
term-llm ask --provider copilot "Explain this code"
```

Credentials are stored in `$XDG_CONFIG_HOME/term-llm/copilot_oauth.json` (normally `~/.config/term-llm/copilot_oauth.json`) and refreshed automatically. The shipped default model is `gpt-4.1`; it is a fallback selection, not a promise that every account or organization enables it.

```yaml
default_provider: copilot
providers:
  copilot:
    model: gpt-4.1
```

Choose a different model from the authenticated catalog when needed. Plan limits, organization policy, and model availability change independently of term-llm releases; do not use a static “free vs paid models” table as an entitlement check. The adapter uses model-specific Responses, Chat Completions, or Messages paths as supported by the catalog, with term-llm's normal tool flow.

Copilot chat credentials are separate from the GitHub billing credentials used by `term-llm usage --provider copilot`; see [Usage tracking](/reference/usage-tracking/).
