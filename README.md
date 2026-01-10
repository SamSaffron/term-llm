# term-llm

A Swiss Army knife for your terminal—AI-powered commands, answers, and images at your fingertips.

[![Release](https://img.shields.io/github/v/release/samsaffron/term-llm?style=flat-square)](https://github.com/samsaffron/term-llm/releases)

## Features

- **Command suggestions**: Natural language → executable shell commands
- **Ask questions**: Get answers with optional web search
- **File editing**: Edit code with AI assistance (supports line ranges)
- **File context**: Include files, clipboard, stdin, or line ranges as context (`-f`)
- **Image generation**: Create and edit images (Gemini, OpenAI, Flux)
- **Multiple providers**: Anthropic, OpenAI, OpenRouter, Gemini, Zen (free tier), Claude CLI, Ollama, LM Studio
- **Local LLMs**: Run with Ollama, LM Studio, or any OpenAI-compatible server
- **Credential reuse**: Works with Claude Code, Codex, gemini-cli credentials

```
$ term-llm exec "find all go files modified today"

> find . -name "*.go" -mtime 0   Uses find with name pattern
  fd -e go --changed-within 1d   Uses fd (faster alternative)
  find . -name "*.go" -newermt "today"   Alternative find syntax
  something else...
```

## Installation

### One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh
```

Or with options:

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh -s -- --version v0.1.0 --install-dir ~/bin
```

### Go install

```bash
go install github.com/samsaffron/term-llm@latest
```

### Build from source

```bash
git clone https://github.com/samsaffron/term-llm
cd term-llm
go build
```

## Setup

On first run, term-llm will prompt you to choose a provider (Anthropic, OpenAI, OpenRouter, Gemini, Zen, Ollama, or LM Studio).

### Option 1: Use existing CLI credentials (recommended)

If you have [Claude Code](https://claude.ai/code), [Codex](https://github.com/openai/codex), or [gemini-cli](https://github.com/google-gemini/gemini-cli) installed and logged in, term-llm can use those credentials:

```yaml
# In ~/.config/term-llm/config.yaml
providers:
  anthropic:
    credentials: claude      # uses Claude Code credentials

  openai:
    credentials: codex       # uses Codex credentials

  gemini:
    credentials: gemini-cli  # uses gemini-cli OAuth credentials
```

### Option 2: Use API key

Set your API key as an environment variable:

```bash
# For Anthropic
export ANTHROPIC_API_KEY=your-key

# For OpenAI
export OPENAI_API_KEY=your-key

# For OpenRouter
export OPENROUTER_API_KEY=your-key

# For Gemini
export GEMINI_API_KEY=your-key
```

### Option 3: Use OpenRouter

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

### Option 4: Use OpenCode Zen (free tier available)

[OpenCode Zen](https://opencode.ai) provides free access to GLM 4.7 and other models. No API key required for free tier, or set `ZEN_API_KEY` for paid models:

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: zen

providers:
  zen:
    model: glm-4.7-free  # default model (free)
    # api_key: optional - leave empty for free tier, or set for paid models
```

Or use the `--provider` flag:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
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

### Option 5: Use local LLMs (Ollama, LM Studio)

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
```

### Option 6: Use Claude CLI (claude-bin)

If you have [Claude Code](https://claude.ai/code) installed and logged in, you can use the `claude-bin` provider to run inference through the Claude CLI binary. This requires no API key - it uses Claude Code's existing authentication.

```bash
# Use directly via --provider flag (no config needed)
term-llm ask --provider claude-bin "explain this code"
term-llm ask --provider claude-bin:haiku "quick question"  # use haiku model
term-llm ask --provider claude-bin -s "latest news"        # with native web search

# Or configure as default
```

```yaml
# In ~/.config/term-llm/config.yaml
default_provider: claude-bin

providers:
  claude-bin:
    model: sonnet  # opus, sonnet, or haiku
```

**Features:**
- No API key required - uses Claude Code's OAuth authentication
- Native web search support with `-s` flag
- Model selection: `opus`, `sonnet` (default), `haiku`
- Works immediately if Claude Code is installed and logged in

OpenAI-compatible providers support two URL options:
- `base_url`: Base URL (e.g., `https://api.cerebras.ai/v1`) - `/chat/completions` is appended automatically
- `url`: Full URL (e.g., `https://api.cerebras.ai/v1/chat/completions`) - used as-is without appending

Use `url` when your endpoint doesn't follow the standard `/chat/completions` path, or to paste URLs directly from API documentation.

## Usage

```bash
term-llm exec "your request here"
```

Use arrow keys to select a command, Enter to execute, or press `h` for detailed help on the highlighted command. Select "something else..." to refine your request.

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | | Override provider, optionally with model (e.g., `openai:gpt-4o`) |
| `--file` | `-f` | File(s) to include as context (supports globs, line ranges, 'clipboard') |
| `--auto-pick` | `-a` | Auto-execute the best suggestion without prompting |
| `--max N` | `-n N` | Limit to N options in the selection UI |
| `--search` | `-s` | Enable web search (DuckDuckGo) and page reading ([Jina AI Reader](https://jina.ai/reader/)) |
| `--native-search` | | Use provider's native search (override config) |
| `--no-native-search` | | Force external search tools instead of native |
| `--print-only` | `-p` | Print the command instead of executing it |
| `--debug` | `-d` | Show provider debug information |
| `--debug-raw` | | Emit raw debug logs with timestamps (tool calls/results, raw requests) |

### Examples

```bash
term-llm exec "list files by size"              # interactive selection
term-llm exec "compress folder" --auto-pick     # auto-execute best
term-llm exec "find large files" -n 3           # show max 3 options
term-llm exec "install latest node" -s          # with web search
term-llm exec "disk usage" -p                   # print only
term-llm exec --provider zen "git status"       # use specific provider
term-llm exec --provider openai:gpt-4o "list"   # provider with specific model
term-llm exec --debug-raw "list files"          # raw debug logs with timestamps
term-llm exec --provider ollama:llama3.2 "list" # use local Ollama model
term-llm exec --provider lmstudio:deepseek "list"  # use LM Studio model
term-llm ask --provider openai:gpt-5.2-xhigh "complex question"  # max reasoning
term-llm exec --provider openai:gpt-5.2-low "quick task"         # faster/cheaper

# With file context
term-llm exec -f error.log "find the cause"     # analyze a file
term-llm exec -f "*.go" "run tests for these"   # glob pattern
git diff | term-llm exec "commit message"       # pipe stdin

# Ask a question
term-llm ask "What is the difference between TCP and UDP?"
term-llm ask "latest node.js version" -s        # with web search
term-llm ask --provider zen "explain docker"    # use specific provider
term-llm ask -f code.go "explain this code"     # with file context
term-llm ask -f code.go:50-100 "explain this function"  # specific lines
term-llm ask -f clipboard "what is this?"       # from clipboard
cat README.md | term-llm ask "summarize this"   # pipe stdin
term-llm ask --debug-raw "latest zig release"   # raw debug logs with timestamps

# Edit files
term-llm edit "add error handling" -f main.go
term-llm edit "refactor loop" -f utils.go:20-40  # only lines 20-40
term-llm edit "add tests" -f "*.go" --dry-run    # preview changes
term-llm edit --debug-raw "fix typos" -f README.md

# Generate images
term-llm image "a sunset over mountains"
term-llm image "logo design" --provider flux    # use specific provider
term-llm image "make it purple" -i photo.png    # edit existing image
```

## Debugging

Use `--debug` to print provider-level diagnostics (requests, model info, etc.). Use `--debug-raw` for a timestamped, raw view of tool calls, tool results, and reconstructed requests. Raw debug is most useful for troubleshooting tool calling and search.

## Image Generation

Generate and edit images using AI models from Gemini, OpenAI, or Flux (Black Forest Labs).

```bash
term-llm image "a robot cat on a rainbow"
```

By default, images are:
- Saved to `~/Pictures/term-llm/` with timestamped filenames
- Displayed in terminal via `icat` (if available)
- Copied to clipboard (actual image data, pasteable in apps)

### Image Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--input` | `-i` | Input image to edit |
| `--provider` | | Override provider (gemini, openai, flux) |
| `--output` | `-o` | Custom output path |
| `--no-display` | | Skip terminal display |
| `--no-clipboard` | | Skip clipboard copy |
| `--no-save` | | Don't save to default location |
| `--debug` | `-d` | Show debug information |

### Image Examples

```bash
# Generate
term-llm image "cyberpunk cityscape at night"
term-llm image "minimalist logo" --provider flux
term-llm image "watercolor painting" -o ./art.png

# Edit existing image
term-llm image "add a hat" -i photo.png
term-llm image "make it look vintage" -i input.png --provider gemini
term-llm image "add sparkles" -i clipboard       # edit from clipboard

# Options
term-llm image "portrait" --no-clipboard        # don't copy to clipboard
term-llm image "landscape" --no-display         # don't show in terminal
```

### Image Providers

| Provider | Model | Environment Variable | Config Key |
|----------|-------|---------------------|------------|
| Gemini (default) | gemini-2.5-flash-image | `GEMINI_API_KEY` | `image.gemini.api_key` |
| OpenAI | gpt-image-1 | `OPENAI_API_KEY` | `image.openai.api_key` |
| Flux | flux-2-pro / flux-kontext-pro | `BFL_API_KEY` | `image.flux.api_key` |

Image providers use their own credentials, separate from text providers. This allows using different API keys or accounts for text vs image generation.

## File Editing

Edit files using natural language instructions:

```bash
term-llm edit "add error handling" --file main.go
term-llm edit "refactor to use interfaces" --file "*.go"
term-llm edit "fix the bug" --file utils.go:45-60     # only lines 45-60
```

### Edit Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--file` | `-f` | File(s) to edit (required, supports globs) |
| `--dry-run` | | Preview changes without applying |
| `--provider` | | Override provider (e.g., `openai:gpt-5.2-codex`) |
| `--per-edit` | | Prompt for each edit separately |
| `--debug` | `-d` | Show debug information |

### Line Range Syntax

Both `edit` and `ask` support line range syntax to focus on specific parts of a file:

```bash
# Edit specific lines
term-llm edit "fix this" --file main.go:11-22    # lines 11 to 22
term-llm edit "fix this" --file main.go:11-      # line 11 to end
term-llm edit "fix this" --file main.go:-22      # start to line 22

# Ask about specific lines
term-llm ask -f main.go:50-100 "explain this function"
```

### Diff Format

term-llm supports two edit strategies:

| Format | Description | Best For |
|--------|-------------|----------|
| `replace` | Multiple parallel find/replace tool calls | Most models (default) |
| `udiff` | Single unified diff with elision support | Codex models, large refactors |

The `udiff` format uses unified diff syntax with `-...` elision to efficiently replace large code blocks without listing every line:

```diff
--- file.go
+++ file.go
@@ func BigFunction @@
-func BigFunction() error {
-...
-}
+func BigFunction() error {
+    return newImpl()
+}
```

Configure in `~/.config/term-llm/config.yaml`:

```yaml
edit:
  diff_format: auto  # auto, udiff, or replace
```

- `auto` (default): Uses `udiff` for Codex models, `replace` for others
- `udiff`: Always use unified diff format
- `replace`: Always use multiple find/replace calls

## Shell Integration (Recommended)

Commands run by term-llm don't appear in your shell history. To fix this, add a shell function that uses `--print-only` mode.

### Zsh

Add to `~/.zshrc`:

```zsh
tl() {
  local cmd=$(term-llm exec --print-only "$@")
  if [[ -n "$cmd" ]]; then
    print -s "$cmd"  # add to history
    eval "$cmd"
  fi
}
```

### Bash

Add to `~/.bashrc`:

```bash
tl() {
  local cmd=$(term-llm exec --print-only "$@")
  if [[ -n "$cmd" ]]; then
    history -s "$cmd"  # add to history
    eval "$cmd"
  fi
}
```

Then use `tl` instead of `term-llm`:

```bash
tl "find large files"
tl "install latest docker" -s      # with web search
tl "compress this folder" -a       # auto-pick best
```

## Configuration

```bash
term-llm config        # Show current config
term-llm config edit   # Edit config file
term-llm config path   # Print config file path
```

## Version & Updates

term-llm automatically checks for updates once per day and notifies you when a new version is available.

```bash
term-llm version       # Show version info
term-llm upgrade       # Upgrade to latest version
term-llm upgrade --version v0.2.0  # Install specific version
```

To disable update checks, set `TERM_LLM_SKIP_UPDATE_CHECK=1`.

Config is stored at `~/.config/term-llm/config.yaml`:

```yaml
default_provider: anthropic

providers:
  # Built-in providers - type is inferred from the key name
  anthropic:
    model: claude-sonnet-4-5
    credentials: claude  # or "api_key" (default)

  openai:
    model: gpt-5.2
    credentials: codex  # or "api_key" (default)

  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm

  gemini:
    model: gemini-3-flash-preview
    credentials: gemini-cli  # or "api_key" (default)

  zen:
    model: glm-4.7-free
    # api_key is optional - leave empty for free tier

  # Local LLM providers (require explicit type)
  # Run 'term-llm models --provider ollama' to list available models
  # ollama:
  #   type: openai_compatible
  #   base_url: http://localhost:11434/v1
  #   model: llama3.2:latest

  # Custom OpenAI-compatible endpoints
  # cerebras:
  #   type: openai_compatible
  #   base_url: https://api.cerebras.ai/v1  # /chat/completions appended automatically
  #   # url: https://api.cerebras.ai/v1/chat/completions  # alternative: full URL, used as-is
  #   model: llama-4-scout-17b
  #   api_key: ${CEREBRAS_API_KEY}

exec:
  suggestions: 3  # number of command suggestions
  # provider: openai    # override provider for exec only
  # model: gpt-4o       # override model for exec only
  instructions: |
    I use Arch Linux with zsh.
    I prefer ripgrep over grep, fd over find.

ask:
  # provider: anthropic
  # model: claude-opus-4  # use a smarter model for questions
  instructions: |
    Be concise. I'm an experienced developer.

edit:
  # provider: openai
  # model: gpt-5.2-codex  # Codex models are optimized for code edits
  diff_format: auto  # auto, udiff, or replace

image:
  provider: gemini  # gemini, openai, or flux
  output_dir: ~/Pictures/term-llm

  gemini:
    api_key: ${GEMINI_API_KEY}
    # model: gemini-2.5-flash-image

  openai:
    api_key: ${OPENAI_API_KEY}
    # model: gpt-image-1

  flux:
    api_key: ${BFL_API_KEY}
    # model: flux-2-pro
```

### Per-Command Provider/Model

Each command (exec, ask, edit) can have its own provider and model, overriding the global default:

```yaml
default_provider: anthropic  # global default

providers:
  anthropic:
    model: claude-sonnet-4-5
  openai:
    model: gpt-5.2
  zen:
    model: glm-4.7-free

exec:
  provider: zen       # exec uses Zen (free)
  model: glm-4.7-free

ask:
  model: claude-opus-4  # ask uses global provider with a smarter model

edit:
  provider: openai
  model: gpt-4o       # edit uses OpenAI
```

**Precedence** (highest to lowest):
1. CLI flag: `--provider openai:gpt-4o`
2. Per-command config: `exec.provider` / `exec.model`
3. Global config: `default_provider` + `providers.<name>.model`

### Reasoning Effort (OpenAI)

For OpenAI models, you can control reasoning effort by appending `-low`, `-medium`, `-high`, or `-xhigh` to the model name:

```bash
term-llm ask --provider openai:gpt-5.2-xhigh "complex question"  # max reasoning
term-llm exec --provider openai:gpt-5.2-low "quick task"         # faster/cheaper
```

Or in config:
```yaml
providers:
  openai:
    model: gpt-5.2-high  # effort parsed from suffix
```

| Effort | Description |
|--------|-------------|
| `low` | Faster, cheaper, less thorough |
| `medium` | Balanced (default if not specified) |
| `high` | More thorough reasoning |
| `xhigh` | Maximum reasoning (only on gpt-5.2) |

### Extended Thinking (Anthropic)

For Anthropic models, you can enable extended thinking by appending `-thinking` to the model name:

```bash
term-llm ask --provider anthropic:claude-sonnet-4-5-thinking "complex question"
```

Or in config:
```yaml
providers:
  anthropic:
    model: claude-sonnet-4-5-thinking  # enables 10k token thinking budget
```

Extended thinking allows Claude to reason through complex problems before responding. The thinking process uses ~10,000 tokens and is not shown in the output.

### Web Search

When using `-s`/`--search`, some providers (Anthropic, OpenAI, Gemini) have native web search built-in. Others use external tools (DuckDuckGo + Jina Reader).

You can force external search even for providers with native support—useful for consistency, debugging, or when native search doesn't work well for your use case.

**CLI flags:**
```bash
term-llm ask "latest news" -s --no-native-search  # Force external search tools
term-llm ask "latest news" -s --native-search     # Force native (override config)
```

**Per-provider config:**
```yaml
providers:
  gemini:
    model: gemini-2.5-flash
    use_native_search: false  # Always use external search for this provider

  anthropic:
    model: claude-sonnet-4-5
    # use_native_search: true  # Default: use native if available
```

**Priority** (highest to lowest):
1. CLI flag: `--native-search` or `--no-native-search`
2. Provider config: `use_native_search: false`
3. Default: use native search if provider supports it

### Credentials

Each provider supports a `credentials` field:

| Provider | Value | Description |
|----------|-------|-------------|
| All | `api_key` | Use environment variable (default) |
| Anthropic | `claude` | Use Claude Code credentials |
| OpenAI | `codex` | Use Codex CLI credentials |
| Gemini | `gemini-cli` | Use gemini-cli OAuth credentials |
| Zen | `api_key` | Optional: empty for free tier, or set `ZEN_API_KEY` for paid models |

**Claude Code** (`credentials: claude`):
- **macOS**: System keychain (via `security` command)
- **Linux**: `~/.claude/.credentials.json`

**Codex** (`credentials: codex`):
- Reads from `~/.codex/auth.json`

**gemini-cli** (`credentials: gemini-cli`):
- Reads OAuth credentials from `~/.gemini/oauth_creds.json`
- Uses Google Code Assist API (same backend as gemini-cli)

### Dynamic Configuration

For advanced setups, term-llm supports dynamic resolution of API keys and URLs using special prefixes. These are resolved lazily—only when actually making an API call, not when loading config.

#### 1Password Integration (`op://`)

Retrieve API keys from 1Password using secret references:

```yaml
providers:
  my-provider:
    type: openai_compatible
    base_url: https://api.example.com/v1
    api_key: "op://Private/My API Key/credential"
```

For multiple 1Password accounts, use the `?account=` query parameter:

```yaml
providers:
  work-llm:
    type: openai_compatible
    base_url: https://llm.company.com/v1
    api_key: "op://Engineering/LLM Service/api_key?account=company.1password.com"
```

This requires the [1Password CLI](https://developer.1password.com/docs/cli/) (`op`) to be installed and signed in.

#### DNS SRV Records (`srv://`)

Discover server endpoints dynamically via DNS SRV records:

```yaml
providers:
  internal-llm:
    type: openai_compatible
    url: "srv://_llm._tcp.internal.company.com/v1/chat/completions"
    api_key: ${LLM_API_KEY}
```

The SRV record is resolved to `https://host:port/path`. This is useful for:
- Load-balanced services with multiple backends
- Internal services with dynamic IPs
- Kubernetes services exposed via external-dns

#### Shell Commands (`$()`)

Execute arbitrary shell commands to get values:

```yaml
providers:
  vault-backed:
    type: openai_compatible
    base_url: https://api.example.com/v1
    api_key: "$(vault kv get -field=api_key secret/llm)"

  aws-secrets:
    type: openai_compatible
    base_url: https://api.example.com/v1
    api_key: "$(aws secretsmanager get-secret-value --secret-id llm-key --query SecretString --output text)"
```

#### Combined Example

Using SRV discovery with 1Password credentials:

```yaml
providers:
  production-llm:
    type: openai_compatible
    model: "Qwen/Qwen3-30B-A3B"
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

When you run `term-llm config`, these show as `[set via 1password]` or `[set via command]` without actually resolving the values (no 1Password prompt until you make an API call).

### Diagnostics

Enable diagnostic logging to capture detailed information when edits fail and retry. This is useful for debugging and tuning prompts:

```yaml
diagnostics:
  enabled: true
  # dir: /custom/path  # optional, defaults to ~/.local/share/term-llm/diagnostics/
```

When an edit fails and retries, two files are written:
- `edit-retry-{timestamp}.json` - Structured data for programmatic analysis
- `edit-retry-{timestamp}.md` - Human-readable with syntax-highlighted code blocks

Each diagnostic captures:
- Provider and model used
- Full system and user prompts
- LLM's partial response before failure
- Failed search pattern or diff
- Current file content
- Error reason

### Shell Completions

Generate and install shell completions:

```bash
term-llm config completion zsh --install   # Install for zsh
term-llm config completion bash --install  # Install for bash
term-llm config completion fish --install  # Install for fish
```

## License

MIT
