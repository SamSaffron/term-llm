---
title: "Providers and setup"
weight: 3
description: "Choose a provider, authenticate, discover available models, and configure the first usable setup."
kicker: "Providers"
source_readme_heading: "Setup"
featured: true
next:
  label: Usage guide
  url: /guides/usage/
---
On first run, term-llm will prompt you to choose a provider (Anthropic, AWS Bedrock, OpenAI, ChatGPT, GitHub Copilot, xAI, Venice, OpenRouter, Gemini, Gemini CLI, Zen, Claude Code (claude-bin), Ollama, or LM Studio).

### Option 1: Try it free with Zen

[OpenCode Zen](https://opencode.ai) provides free access to multiple models. No API key required:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
term-llm ask --provider zen:gpt-5-nano "quick question"  # use specific model
```

**Available free models:** `glm-4.7-free`, `minimax-m2.1-free`, `grok-code`, `big-pickle`, `gpt-5-nano`

Or configure as default:

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: zen
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

# For OpenRouter
export OPENROUTER_API_KEY=your-key

# For Gemini
export GEMINI_API_KEY=your-key

# For Perplexity search (used by search.provider: perplexity)
export PERPLEXITY_API_KEY=your-key
```

### Option 3: Use ChatGPT (Plus/Pro subscription)

If you have a ChatGPT Plus or Pro subscription, you can use the `chatgpt` provider with native OAuth authentication for both text and image workflows:

```bash
term-llm ask --provider chatgpt "explain this code"
term-llm ask --provider chatgpt:gpt-5.2-codex "code question"
term-llm image --provider chatgpt:gpt-5.4 "storybook fox in the snow"
```

On first use, you'll be prompted to authenticate via browser. Credentials are stored locally and refreshed automatically.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: chatgpt

providers:
  chatgpt:
    model: gpt-5.2-codex
```

### Option 4: Use xAI (Grok)

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

### Option 6: Use OpenRouter

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

### Option 7: Use AWS Bedrock

[AWS Bedrock](https://aws.amazon.com/bedrock/) provides access to Anthropic Claude models through your AWS account. This is useful for organizations that route AI usage through AWS billing, need VPC/PrivateLink access, or use application inference profiles for rate/cost management.

```bash
term-llm ask --provider bedrock "explain this code"
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

**Application inference profiles** — use `model_map` to alias friendly model names to Bedrock ARNs or model IDs. The `-thinking` and `-1m` suffixes work with mapped names:

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

**Available models** (same as direct Anthropic — translated to Bedrock IDs automatically):

| Model | Suffixes | Description |
|-------|----------|-------------|
| `claude-sonnet-4-6` | `-thinking`, `-1m` | Latest Sonnet |
| `claude-opus-4-6` | `-thinking`, `-1m` | Latest Opus |
| `claude-haiku-4-5` | `-thinking` | Fast, lightweight |

The geographic prefix (`us.`, `eu.`, `ap.`) is derived from your configured region — `eu-west-1` produces `eu.anthropic.*` IDs, etc. This ensures data residency matches your region.

You can also pass raw Bedrock model IDs directly (e.g., `us.anthropic.claude-sonnet-4-6` or full ARNs) — these bypass the translation layer.

**Features:** full parity with the direct Anthropic provider — streaming, tool use, extended thinking, 1M context, images, prompt caching, web search/fetch all work through Bedrock.

| Config field | Description |
|---|---|
| `region` | AWS region. Falls back to `AWS_REGION` / `AWS_DEFAULT_REGION` env vars, then `us-east-1`. |
| `profile` | AWS profile name from `~/.aws/credentials`. |
| `access_key_id` | Explicit AWS access key. Supports `$()`, `op://`, `${ENV}` resolution. |
| `secret_access_key` | Explicit AWS secret key. Same resolution support. |
| `session_token` | Optional session token for temporary/assumed-role credentials. |
| `model_map` | Map of friendly names to Bedrock model IDs or ARNs. |
| `model` | Default model (friendly name, Bedrock ID, or ARN). |

### Option 8: Use local LLMs (Ollama, LM Studio)

Run models locally with [Ollama](https://ollama.com) or [LM Studio](https://lmstudio.ai):

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
    type: openai_compatible
    base_url: http://localhost:11434/v1
    model: llama3.2:latest

  lmstudio:
    type: openai_compatible
    base_url: http://localhost:1234/v1
    model: deepseek-coder-v2
```

For other OpenAI-compatible servers (vLLM, text-generation-inference, etc.):

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

See [Providers and models](/reference/providers-and-models/#configuration-reference) for the full list of OpenAI-compatible provider options.

### Option 9: Use Claude Code (claude-bin)

If you have [Claude Code](https://claude.ai/code) installed and logged in, you can use the `claude-bin` provider to run completions via the [Claude Agent SDK](https://docs.anthropic.com/en/docs/claude-code/sdk). This requires no API key - it uses Claude Code's existing authentication.

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
    model: sonnet  # opus, sonnet, or haiku
    env:
      IS_SANDBOX: "1"  # useful when running term-llm as root in a trusted container
      # Generate a long-lived token with: claude setup-token
      # Useful in CI or headless environments where interactive login isn't possible
      CLAUDE_CODE_OAUTH_TOKEN: "your-oauth-token-here"
    # Optional: Claude Code hooks are disabled by default; set to true to opt back in
    # enable_hooks: true
```

**Features:**
- No API key required - uses Claude Code's existing authentication
- Full tool support via MCP (exec, search, edit all work)
- Model selection: `opus`, `sonnet` (default), `haiku`
- Claude Code hooks are disabled by default to keep user hook automation out of term-llm inference sessions
- Optional `providers.claude-bin.enable_hooks: true` to opt back into Claude Code hooks
- Optional `providers.claude-bin.env` passthrough for Claude subprocess settings (for example `IS_SANDBOX=1` in trusted root-run containers)
- `providers.<name>.env` values support the same deferred resolution as other config values, including `file://...#json.path`, `op://...`, and `$()`
- Works immediately if Claude Code is installed and logged in

### Option 10: Use existing CLI credentials (gemini-cli)

If you have [gemini-cli](https://github.com/google-gemini/gemini-cli) installed and logged in, term-llm can use those credentials directly:

```bash
# Use gemini-cli credentials (no config needed)
term-llm ask --provider gemini-cli "explain this code"
```

Or configure as default:

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: gemini-cli  # uses ~/.gemini/oauth_creds.json
```

OpenAI-compatible providers support two URL options:
- `base_url`: Base URL (e.g., `https://api.cerebras.ai/v1`) - `/chat/completions` is appended automatically
- `url`: Full URL (e.g., `https://api.cerebras.ai/v1/chat/completions`) - used as-is without appending

Use `url` when your endpoint doesn't follow the standard `/chat/completions` path, or to paste URLs directly from API documentation.

### Option 11: Use GitHub Copilot

If you have [GitHub Copilot](https://github.com/features/copilot) (free, Individual, or Business), you can use the `copilot` provider with OAuth device flow authentication:

```bash
term-llm ask --provider copilot "explain this code"
term-llm ask --provider copilot:claude-opus-4.5 "complex question"
```

On first use, you'll be prompted to authenticate via GitHub device flow. Credentials are stored locally and refreshed automatically.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: copilot

providers:
  copilot:
    model: gpt-4.1  # free tier, or gpt-5.2-codex for paid
```

**Available models:**
| Model | Description |
|-------|-------------|
| `gpt-4.1` | Default, works on free tier |
| `gpt-5.2-codex` | Advanced coding model (paid) |
| `gpt-5.1` | GPT-5.1 (paid) |
| `claude-opus-4.5` | Claude Opus 4.5 via Copilot (paid) |
| `gemini-3-pro` | Gemini 3 Pro via Copilot (paid) |
| `grok-code-fast-1` | Grok coding model via Copilot (paid) |

**Features:**
- Free tier with GPT-4.1 (no Copilot subscription required, just GitHub account)
- OAuth device flow authentication (no API key needed)
- Full tool support via MCP
