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
On first run, term-llm will prompt you to choose a provider (Anthropic, OpenAI, ChatGPT, GitHub Copilot, xAI, Venice, OpenRouter, Gemini, Gemini CLI, Zen, Claude Code (claude-bin), Ollama, or LM Studio).

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
# For Anthropic (API key, or use OAuth — see below)
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

### Option 3: Use Anthropic with OAuth (Claude Pro/Max subscription)

If you have a Claude Pro or Max subscription and the [Claude Code CLI](https://claude.ai/code) installed, you can use OAuth instead of an API key:

```bash
term-llm ask --provider anthropic "explain this code"
```

On first interactive use, you'll be prompted to run `claude setup-token` and paste the resulting token. The token is saved to `~/.config/term-llm/anthropic_oauth.json` and reused automatically.

You can also set the token via environment variable (useful for CI after generating a token interactively):

```bash
export CLAUDE_CODE_OAUTH_TOKEN=your-oauth-token
```

### Option 4: Use ChatGPT (Plus/Pro subscription)

If you have a ChatGPT Plus or Pro subscription, you can use the `chatgpt` provider with native OAuth authentication:

```bash
term-llm ask --provider chatgpt "explain this code"
term-llm ask --provider chatgpt:gpt-5.2-codex "code question"
```

On first use, you'll be prompted to authenticate via browser. Credentials are stored locally and refreshed automatically.

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: chatgpt

providers:
  chatgpt:
    model: gpt-5.2-codex
```

### Option 5: Use xAI (Grok)

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

### Option 6: Use Venice

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

### Option 7: Use OpenRouter

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
      CLAUDE_CODE_OAUTH_TOKEN: "file:///root/.config/term-llm/anthropic_oauth.json#access_token"
```

**Features:**
- No API key required - uses Claude Code's OAuth authentication
- Full tool support via MCP (exec, search, edit all work)
- Model selection: `opus`, `sonnet` (default), `haiku`
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
