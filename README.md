<p align="center">
  <img src="assets/logo.png" alt="term-llm logo" width="200">
</p>

# term-llm

A Swiss Army knife for your terminal—AI-powered commands, answers, and images at your fingertips.

[![Release](https://img.shields.io/github/v/release/samsaffron/term-llm?style=flat-square)](https://github.com/samsaffron/term-llm/releases)

## Features

- **Command suggestions**: Natural language → executable shell commands
- **Ask questions**: Get answers with optional web search
- **Chat mode**: Persistent sessions with tool and MCP support
- **File editing**: Edit code with AI assistance (supports line ranges)
- **File context**: Include files, clipboard, stdin, or line ranges as context (`-f`)
- **Image generation**: Create and edit images (Gemini, OpenAI, xAI, Flux)
- **Text embeddings**: Generate vector embeddings for search, RAG, and similarity (Gemini, OpenAI, Jina, Voyage, Ollama)
- **MCP servers**: Extend with external tools via [Model Context Protocol](https://modelcontextprotocol.io)
- **Agents**: Named configuration bundles for different workflows
- **Skills**: Portable instruction bundles for specialized tasks
- **Multiple providers**: Anthropic, OpenAI, ChatGPT, GitHub Copilot, xAI (Grok), OpenRouter, Gemini, Gemini CLI, Zen (free tier), Claude Code (claude-bin), Ollama, LM Studio
- **Local LLMs**: Run with Ollama, LM Studio, or any OpenAI-compatible server
- **Free tier available**: Try it out with Zen (no API key required)

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

On first run, term-llm will prompt you to choose a provider (Anthropic, OpenAI, ChatGPT, GitHub Copilot, xAI, OpenRouter, Gemini, Gemini CLI, Zen, Claude Code (claude-bin), Ollama, or LM Studio).

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

# For OpenRouter
export OPENROUTER_API_KEY=your-key

# For Gemini
export GEMINI_API_KEY=your-key
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

### Option 7: Use local LLMs (Ollama, LM Studio)

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

### Option 8: Use Claude Code (claude-bin)

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
```

**Features:**
- No API key required - uses Claude Code's OAuth authentication
- Full tool support via MCP (exec, search, edit all work)
- Model selection: `opus`, `sonnet` (default), `haiku`
- Works immediately if Claude Code is installed and logged in

### Option 9: Use existing CLI credentials (gemini-cli)

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

### Option 10: Use GitHub Copilot

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

## Usage

```bash
term-llm exec "your request here"
```

Use arrow keys to select a command, Enter to execute, or press `h` for detailed help on the highlighted command. Select "something else..." to refine your request.

Use `term-llm chat` for a persistent session.

```bash
term-llm chat
```

### Using Agents

Use the `@agent` prefix syntax to use a specific agent:

```bash
term-llm ask @reviewer "review this code"     # use reviewer agent
term-llm chat @coder                          # start chat with coder agent
term-llm loop @researcher --done-file ...     # use researcher agent in loop
term-llm exec @bash-expert "find large files" # use bash-expert agent
```

See [Agents](#agents) for more details on creating and managing agents.

### Chat Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Enter` | Send message |
| `Ctrl+J` or `Alt+Enter` | Insert newline |
| `Ctrl+C` | Quit |
| `Ctrl+K` | Clear conversation |
| `Ctrl+S` | Toggle web search |
| `Ctrl+P` | Command palette |
| `Ctrl+T` | MCP server picker |
| `Ctrl+L` | Switch model |
| `Ctrl+N` | New session |
| `Ctrl+F` | Attach file |
| `Ctrl+O` | Conversation inspector |
| `Esc` | Cancel streaming |

### Chat Slash Commands

| Command | Description |
|---------|-------------|
| `/help` | Show help |
| `/clear` | Clear conversation |
| `/model` | Show current model |
| `/search` | Toggle web search |
| `/mcp` | Manage MCP servers |
| `/quit` | Exit chat |

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | | Override provider, optionally with model (e.g., `openai:gpt-5.2`) |
| `--file` | `-f` | File(s) to include as context (supports globs, line ranges, 'clipboard') |
| `--auto-pick` | `-a` | Auto-execute the best suggestion without prompting (exec only) |
| `--agent` | `-a` | Use a specific agent (ask/chat only; see also `@agent` syntax) |
| `--skills` | | Skills mode: all, none, or comma-separated names |
| `--max N` | `-n N` | Limit to N options in the selection UI |
| `--search` | `-s` | Enable web search (configurable: Exa, Brave, Google, DuckDuckGo) and page reading |
| `--native-search` | | Use provider's native search (override config) |
| `--no-native-search` | | Force external search tools instead of native |
| `--print-only` | `-p` | Print the command instead of executing it |
| `--debug` | `-d` | Show provider debug information |
| `--debug-raw` | | Emit raw debug logs with timestamps (tool calls/results, raw requests) |
| `--system-message` | `-m` | Custom system message/instructions |
| `--stats` | | Show session statistics (time, tokens, tool calls) |
| `--no-session` | | Disable session persistence for this command |
| `--session-db` | | Override sessions database path (supports `:memory:`) |
| `--max-turns` | | Max agentic turns for tool execution (default: 20 for exec, 200 for chat) |
| `--yolo` | | Auto-approve all tool operations (for unattended runs) |

**Note:** The `-a` short flag has different meanings:
- In `exec`: `-a` is `--auto-pick` (auto-execute best suggestion)
- In `ask`/`chat`: `-a` is `--agent` (use a specific agent)

### Examples

```bash
term-llm exec "list files by size"              # interactive selection
term-llm exec "compress folder" --auto-pick     # auto-execute best
term-llm exec "find large files" -n 3           # show max 3 options
term-llm exec "install latest node" -s          # with web search
term-llm exec "disk usage" -p                   # print only
term-llm exec --provider zen "git status"       # use specific provider
term-llm exec --provider openai:gpt-5.2 "list"   # provider with specific model
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
term-llm edit "use the API" -f main.go -c api/client.go  # with context

# Generate images
term-llm image "a sunset over mountains"
term-llm image "logo design" --provider flux    # use specific provider
term-llm image "make it purple" -i photo.png    # edit existing image
```

## Debugging

Use `--debug` to print provider-level diagnostics (requests, model info, etc.). Use `--debug-raw` for a timestamped, raw view of tool calls, tool results, and reconstructed requests. Raw debug is most useful for troubleshooting tool calling and search.

### Debug Logging

term-llm maintains debug logs for troubleshooting. Use the `debug-log` command to view and manage them:

```bash
term-llm debug-log                           # Show recent logs
term-llm debug-log list                      # List available log files
term-llm debug-log show [file]               # Show a specific log file
term-llm debug-log tail                      # Show last N lines
term-llm debug-log tail --follow             # Follow logs in real-time
term-llm debug-log search "pattern"          # Search logs for a pattern
term-llm debug-log clean                     # Clean old log files
term-llm debug-log clean --days 7            # Keep only last 7 days
term-llm debug-log export --json             # Export logs as JSON
term-llm debug-log enable                    # Enable debug logging
term-llm debug-log disable                   # Disable debug logging
term-llm debug-log status                    # Show logging status
term-llm debug-log path                      # Print log directory path
```

**Key flags:**
| Flag | Description |
|------|-------------|
| `--days N` | Limit to logs from last N days |
| `--show-tools` | Include tool calls/results in output |
| `--raw` | Show raw log entries without formatting |
| `--json` | Output as JSON |
| `--follow` | Follow logs in real-time (with tail) |

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
term-llm image "futuristic city" --provider xai  # uses Grok image model
term-llm image "watercolor painting" -o ./art.png

# Edit existing image (not supported by xAI)
term-llm image "add a hat" -i photo.png
term-llm image "make it look vintage" -i input.png --provider gemini
term-llm image "add sparkles" -i clipboard       # edit from clipboard

# Options
term-llm image "portrait" --no-clipboard        # don't copy to clipboard
term-llm image "landscape" --no-display         # don't show in terminal
```

### Image Providers

| Provider | Models | Environment Variable | Config Key |
|----------|--------|---------------------|------------|
| Gemini (default) | gemini-2.5-flash-image | `GEMINI_API_KEY` | `image.gemini.api_key` |
| OpenAI | gpt-image-1, gpt-image-1.5, gpt-image-1-mini | `OPENAI_API_KEY` | `image.openai.api_key` |
| xAI | grok-2-image-1212 | `XAI_API_KEY` | `image.xai.api_key` |
| Flux | flux-2-pro, flux-2-max, flux-kontext-pro | `BFL_API_KEY` | `image.flux.api_key` |
| OpenRouter | various | `OPENROUTER_API_KEY` | `image.openrouter.api_key` |

Image providers use their own credentials, separate from text providers. This allows using different API keys or accounts for text vs image generation.

**Note:** xAI image generation does not support image editing (`-i` flag).

## Text Embeddings

Generate vector embeddings from text for semantic search, RAG, clustering, and similarity comparison.

```bash
term-llm embed "What is the meaning of life?"
```

Embeddings are numerical representations of text (arrays of floats) that capture semantic meaning. The `embed` command takes text input, calls an embedding API, and outputs vectors.

### Embed Input Methods

```bash
# Positional arguments (each embedded separately)
term-llm embed "first text" "second text" "third text"

# From stdin
echo "Hello world" | term-llm embed

# From files
term-llm embed -f document.txt
term-llm embed -f doc1.txt -f doc2.txt

# Mixed
term-llm embed "query text" -f corpus.txt
```

### Embed Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | `-p` | Override provider (gemini, openai, jina, voyage, ollama) with optional model |
| `--file` | `-f` | Input file(s) to embed (repeatable) |
| `--format` | | Output format: `json` (default), `array`, `plain` |
| `--output` | `-o` | Write output to file |
| `--dimensions` | | Custom output dimensions (Matryoshka truncation) |
| `--task-type` | | Task type hint (e.g., RETRIEVAL_QUERY, RETRIEVAL_DOCUMENT, SEMANTIC_SIMILARITY) |
| `--similarity` | | Compare texts by cosine similarity instead of outputting vectors |

### Embed Output Formats

```bash
# JSON with metadata (default)
term-llm embed "hello"
# → {"model": "gemini-embedding-001", "dimensions": 3072, "embeddings": [...]}

# Bare JSON array(s) — one per input, for piping
term-llm embed "hello" --format array
# → [0.0023, -0.0094, 0.0156, ...]

# One number per line (single input only)
term-llm embed "hello" --format plain
# → 0.0023
#   -0.0094
#   ...

# Save to file
term-llm embed "hello" -o embeddings.json
```

### Similarity Mode

Compare texts by cosine similarity without manually handling vectors:

```bash
# Pairwise comparison
term-llm embed --similarity "king" "queen"
# → 0.834521

# Rank multiple texts against a query (first argument)
term-llm embed --similarity "What is AI?" "Machine learning is a subset of AI" "The weather is nice" "Neural networks process data"
# → 1. 0.891234  Machine learning is a subset of AI
#   2. 0.812456  Neural networks process data
#   3. 0.234567  The weather is nice
```

### Embed Examples

```bash
# Provider/model selection
term-llm embed "hello" -p openai                          # use OpenAI
term-llm embed "hello" -p openai:text-embedding-3-large   # specific model
term-llm embed "hello" -p gemini                           # use Gemini
term-llm embed "hello" -p jina                             # use Jina (free tier)
term-llm embed "hello" -p voyage                           # use Voyage AI
term-llm embed "hello" -p ollama:nomic-embed-text          # local Ollama

# Custom dimensions (Matryoshka)
term-llm embed "hello" --dimensions 256

# Gemini task type hints
term-llm embed "search query" --task-type RETRIEVAL_QUERY -p gemini
term-llm embed -f doc.txt --task-type RETRIEVAL_DOCUMENT -p gemini
```

### Embedding Providers

| Provider | Default Model | Dimensions | Environment Variable | Free Tier |
|----------|--------------|------------|---------------------|-----------|
| Gemini (default) | `gemini-embedding-001` | 3072 (128–3072) | `GEMINI_API_KEY` | Yes |
| OpenAI | `text-embedding-3-small` | 1536 (customizable) | `OPENAI_API_KEY` | No |
| [Jina AI](https://jina.ai/embeddings/) | `jina-embeddings-v3` | 1024 (customizable) | `JINA_API_KEY` | Yes (10M tokens) |
| [Voyage AI](https://voyageai.com) | `voyage-3.5` | 1024 (256–2048) | `VOYAGE_API_KEY` | No |
| Ollama | `nomic-embed-text` | 768 | — | Local |

Embedding providers use their own credentials, separate from text and image providers. The default provider is auto-detected from your LLM provider (Gemini users → Gemini, OpenAI users → OpenAI, Anthropic users → Voyage if configured, otherwise Gemini).

**Jina AI** is a great choice for getting started — sign up at [jina.ai/embeddings](https://jina.ai/embeddings/) for a free API key with 10M tokens, no credit card required.

**Voyage AI** is Anthropic's recommended embedding partner (acquired by MongoDB, Feb 2025). The API remains fully available.

## File Editing

Edit files using natural language instructions:

```bash
term-llm edit "add error handling" --file main.go
term-llm edit "refactor to use interfaces" --file "*.go"
term-llm edit "fix the bug" --file utils.go:45-60     # only lines 45-60
term-llm edit "use the API" -f main.go -c api/client.go  # with context files
```

### Edit Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--file` | `-f` | File(s) to edit (required, supports globs) |
| `--context` | `-c` | Read-only reference file(s) (supports globs, 'clipboard') |
| `--dry-run` | | Preview changes without applying |
| `--provider` | | Override provider (e.g., `openai:gpt-5.2-codex`) |
| `--per-edit` | | Prompt for each edit separately |
| `--debug` | `-d` | Show debug information |

### Context Files

Use `--context`/`-c` to include reference files that inform the edit but won't be modified:

```bash
term-llm edit "refactor to use the client" -f handler.go -c api/client.go -c types.go
```

Context files are shown to the AI as read-only references. This is useful when your edit depends on types, interfaces, or patterns defined elsewhere.

You can also pipe stdin as context, which is handy for git diffs:

```bash
git diff | term-llm edit "apply these changes" -f main.go
git show HEAD~1 | term-llm edit "undo this change" -f handler.go
```

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

## Autonomous Loops

Run an agent in a loop until a completion condition is met. State persists in the filesystem—the agent reads/writes files to track progress, and each iteration starts with fresh context.

```bash
# Run until tests pass
term-llm loop --done "go test ./..." --tools all "fix the failing tests"

# Run until file contains marker
term-llm loop --done-file TODO.md:COMPLETE \
  "Implement features in {{TODO.md}}. Mark COMPLETE when done."
```

### Loop Flags

| Flag | Description |
|------|-------------|
| `--done "cmd"` | Exit when command returns 0 |
| `--done-file FILE:TEXT` | Exit when file contains TEXT |
| `--max N` | Maximum iterations (0 = unlimited) |
| `--history N` | Inject last N iteration summaries to avoid repeating mistakes |
| `--yolo` | Auto-approve all tool operations (for unattended runs) |

All standard flags work: `--tools`, `--mcp`, `--agent`, `--provider`, `--search`, etc.

### File Expansion

Use `{{file}}` in your prompt to inline file contents. Files are re-read each iteration, so agents can update them for inter-iteration state:

```bash
term-llm loop --done "npm test" \
  "Implement the spec in {{SPEC.md}}. Track progress in {{TODO.md}}."
```

### History

Use `--history N` to inject summaries of previous iterations. This helps the agent avoid repeating failed approaches:

```bash
term-llm loop --done "go test" --history 3 --tools all \
  "Fix the tests. Don't repeat failed approaches."
```

Each iteration summary includes tools used and a truncated output.

### Examples

```bash
# Migration: run until no class components remain
term-llm loop --done "! grep -r 'React.Component' src/" --tools all \
  "Convert class components to hooks. One file at a time. Run tests after each."

# Research: run until conclusion written
term-llm loop --done-file RESEARCH.md:"## Conclusion" --tools read,write --search --max 20 \
  "Research WebGPU compute shaders. Current progress: {{RESEARCH.md}}. Write a Conclusion section when done."

# With an agent and iteration cap
term-llm loop @coder --done "make build" --max 20 --yolo \
  "Implement the feature described in {{SPEC.md}}"
```

## MCP Servers

[MCP (Model Context Protocol)](https://modelcontextprotocol.io) lets you extend term-llm with external tools—browser automation, database access, API integrations, and more.

```bash
# Add from registry
term-llm mcp add playwright              # search and install
term-llm mcp add @anthropic/mcp-server-fetch

# Add from URL (HTTP transport)
term-llm mcp add https://developers.openai.com/mcp

# Use with any command
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm ask --mcp github "list my open PRs"
term-llm chat --mcp playwright,filesystem
```

### MCP Commands

| Command | Description |
|---------|-------------|
| `mcp add <name-or-url>` | Add server from registry or URL |
| `mcp list` | List configured servers |
| `mcp info <name>` | Show server info and tools |
| `mcp run <server> <tool> [args]` | Run MCP tool(s) directly |
| `mcp remove <name>` | Remove a server |
| `mcp browse [query]` | Browse/search the MCP registry |
| `mcp path` | Print config file path |

### Adding Servers

**From the registry** (stdio transport):
```bash
term-llm mcp add playwright           # search by name
term-llm mcp add @playwright/mcp      # exact package
term-llm mcp browse                   # interactive browser
```

**From a URL** (HTTP transport):
```bash
term-llm mcp add https://developers.openai.com/mcp
term-llm mcp add https://mcp.example.com/api
```

### Using MCP Tools

The `--mcp` flag works with all commands (`ask`, `exec`, `edit`, `chat`):

```bash
# Single server
term-llm ask --mcp fetch "summarize https://example.com"
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm edit --mcp github -f main.go "update based on latest API"

# Multiple servers (comma-separated)
term-llm chat --mcp playwright,filesystem,github

# In chat, toggle servers with Ctrl+M
```

### Running Tools Directly

Use `mcp run` to call MCP tools without going through the LLM:

```bash
# Simple key=value arguments
term-llm mcp run filesystem read_file path=/tmp/test.txt

# JSON arguments for complex values
term-llm mcp run server tool '{"nested":{"deep":"value"}}'

# Multiple tools in one invocation
term-llm mcp run server tool1 key=val tool2 key=val

# Read file contents into a parameter with @path
term-llm mcp run server tool content=@/tmp/big-file.txt

# Read from stdin with @-
cat data.json | term-llm mcp run server tool input=@-
```

### Configuration

MCP servers are stored in `~/.config/term-llm/mcp.json`:

```json
{
  "servers": {
    "playwright": {
      "command": "npx",
      "args": ["-y", "@playwright/mcp"]
    },
    "openai-docs": {
      "type": "http",
      "url": "https://developers.openai.com/mcp"
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_xxx"
      }
    },
    "authenticated-api": {
      "type": "http",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "Bearer your-token"
      }
    }
  }
}
```

### Transport Types

| Type | Config | Description |
|------|--------|-------------|
| stdio | `command` + `args` | Runs as subprocess (npm/pypi packages) |
| http | `url` | Connects to remote HTTP endpoint |

HTTP transport uses [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports) (MCP spec 2025-03-26).

## Agents

Agents are named configuration bundles that define a persona with specific provider, model, system prompt, tools, and MCP servers. Use agents to switch between different workflows quickly.

### Using Agents

Use the `@agent` prefix syntax or `--agent` flag:

```bash
term-llm ask @reviewer "review this code"     # use reviewer agent
term-llm chat @coder                          # start chat with coder agent
term-llm ask --agent reviewer "question"      # alternative syntax
```

### Managing Agents

```bash
term-llm agents                              # List all agents
term-llm agents list                         # Same as above
term-llm agents list --builtin               # Only built-in agents
term-llm agents list --local                 # Only local agents
term-llm agents list --user                  # Only user agents
term-llm agents new my-agent                 # Create new agent
term-llm agents show reviewer                # Show agent configuration
term-llm agents edit reviewer                # Edit agent configuration
term-llm agents copy builtin/coder my-coder  # Copy agent to customize
term-llm agents path                         # Print agents directory
```

### Agent Configuration

Agents are YAML files stored in `~/.config/term-llm/agents/`:

```yaml
# ~/.config/term-llm/agents/reviewer/agent.yaml
name: Code Reviewer
description: Reviews code for best practices and potential issues

provider: anthropic
model: claude-sonnet-4-6

tools:
  enabled: [read_file, grep, glob]
  # OR use a denylist instead:
  # disabled: [shell, write_file]

shell:
  allow: ["git *", "npm test"]  # glob patterns for allowed commands
  auto_run: true                 # skip confirmation for matched commands
  scripts:                       # named shortcuts (auto-approved)
    build: "npm run build"

search: true   # enables web_search and read_url tools
max_turns: 50

mcp:
  - name: github
```

### System Prompt File Includes

System prompts support inline file includes with `{{file:...}}`.

```md
You are a reviewer.

{{file:prompts/rules.md}}
{{file:/absolute/path/to/shared-context.md}}
```

Behavior:
- Includes are expanded recursively (max depth: 10)
- Cycles are detected and reported as errors
- Missing/unreadable include files fail fast
- Included content is inserted raw (no automatic headers/separators)
- Relative paths are source-relative:
  - Agent prompts resolve relative to the agent directory
  - Config/CLI system prompts resolve relative to the current working directory
- Expansion order is include first, then template variables (for example `{{year}}`)

**Agent search order:** user agents → local agents → built-in agents

## Skills

Skills are portable instruction bundles that provide specialized knowledge for specific tasks. Unlike agents, skills don't change the provider or model—they just add context.

### Using Skills

```bash
term-llm ask --skills git "how to squash commits"   # use git skill
term-llm chat --skills git,docker                   # multiple skills
term-llm edit --skills refactoring -f main.go "refactor this"
```

### Managing Skills

```bash
term-llm skills                              # List all skills
term-llm skills list                         # Same as above
term-llm skills list --local                 # Only local skills
term-llm skills list --user                  # Only user skills
term-llm skills new my-skill                 # Create new skill
term-llm skills show git                     # Show skill content
term-llm skills edit git                     # Edit skill
term-llm skills copy builtin/git my-git      # Copy skill to customize
term-llm skills browse                       # Browse available skills
term-llm skills validate my-skill            # Validate skill syntax
term-llm skills update                       # Update skills from sources
term-llm skills path                         # Print skills directory
```

### Skill Configuration

Skills live in `~/.config/term-llm/skills/<name>/SKILL.md`. Each skill is a directory containing a `SKILL.md` file with YAML frontmatter and Markdown body:

```markdown
---
name: git
description: "Git version control expertise"
---

# Git Skill

When helping with Git:
- Prefer rebase over merge for cleaner history
- Use conventional commit messages
- Explain the implications of destructive operations
```

**Skill search order:** local (project) → user (`~/.config/term-llm/skills/`) → built-in

### Skill Tools

Skills can declare script-backed tools in their frontmatter. When the skill is activated via `activate_skill`, those tools are dynamically registered with the engine—the LLM can then call them directly, with no hardcoded paths anywhere.

```markdown
---
name: google-maps
description: "Google Maps queries: travel times, place search, geocoding"
tools:
  - name: maps_travel_time
    description: "Get traffic-aware travel time between two locations"
    script: scripts/travel-time.sh
    timeout_seconds: 15
    input:
      type: object
      properties:
        origin:
          type: string
          description: "Origin address or lat,lng"
        destination:
          type: string
          description: "Destination address or lat,lng"
        mode:
          type: string
          description: "DRIVE, WALK, BICYCLE, or TRANSIT (default: DRIVE)"
      required: [origin, destination]

  - name: maps_places_search
    description: "Free-text place search with optional location bias"
    script: scripts/places-search.sh
    input:
      type: object
      properties:
        query:
          type: string
        latlng:
          type: string
          description: "Optional bias point as lat,lng"
      required: [query]
---

# Google Maps Skill

API key is embedded in the scripts—no need to handle it here.
...
```

Scripts live in the skill directory (e.g. `scripts/travel-time.sh`) and receive the LLM's arguments as **JSON on stdin**, exactly like agent custom tools:

```bash
#!/usr/bin/env bash
INPUT=$(cat)
ORIGIN=$(echo "$INPUT" | jq -r '.origin')
DESTINATION=$(echo "$INPUT" | jq -r '.destination')
MODE=$(echo "$INPUT" | jq -r '.mode // "DRIVE"')
# ... call the API
```

This is the recommended pattern for skills that need API keys or other secrets—the key lives only in the script, never in the SKILL.md body that gets injected into the LLM context.

**Field reference** (same as agent custom tools):

| Field | Required | Description |
|-------|----------|-------------|
| `name` | ✓ | Tool name shown to LLM. Must match `^[a-z][a-z0-9_]*$` |
| `description` | ✓ | Description passed to LLM in the tool spec |
| `script` | ✓ | Path relative to the skill directory (e.g. `scripts/foo.sh`) |
| `input` | | JSON Schema for parameters. Must be `type: object` at root |
| `timeout_seconds` | | Execution timeout (default 30, max 300) |
| `env` | | Extra environment variables when running the script |

Scripts run with `TERM_LLM_AGENT_DIR` set to the skill's directory and `TERM_LLM_TOOL_NAME` set to the tool name. Symlinks are resolved and containment-checked—scripts cannot escape the skill directory.

## Built-in Tools

term-llm includes built-in tools for file operations and shell access. Enable them with the `--tools` flag:

```bash
term-llm chat --tools read_file,shell,grep        # Enable specific tools
term-llm exec --tools read_file,write_file,edit_file,shell,grep,glob,view_image
```

### Available Tools

| Tool | Description |
|------|-------------|
| `read_file` | Read file contents (with line ranges) |
| `write_file` | Create/overwrite files |
| `edit_file` | Edit existing files |
| `shell` | Execute shell commands |
| `grep` | Search file contents (uses ripgrep) |
| `glob` | Find files by glob pattern |
| `view_image` | Display images in terminal (icat) |
| `show_image` | Show image file info |
| `image_generate` | Generate images via configured provider |
| `ask_user` | Prompt user for input |
| `spawn_agent` | Spawn child agents for parallel tasks |
| `run_agent_script` | Run a script bundled in the agent directory |
| `activate_skill` | Activate a skill by name |

### Custom Tools

Agents can declare named, schema-bearing tools backed by shell scripts in the agent directory. These appear to the LLM as first-class tools with their own descriptions and typed parameters—no more asking the LLM to invoke `run_agent_script` with a magic filename.

```yaml
tools:
  enabled: [read_file, shell]
  custom:
    - name: job_status
      description: "List all registered jobs and their last run result."
      script: scripts/job-status.sh

    - name: job_run
      description: "Trigger a scheduled job to run immediately."
      script: scripts/job-run.sh
      input:
        type: object
        properties:
          name:
            type: string
            description: "Job name to run"
        required: [name]
        additionalProperties: false

    - name: job_history
      description: "Fetch recent run history for a job."
      script: scripts/job-history.sh
      input:
        type: object
        properties:
          name:
            type: string
          limit:
            type: integer
            description: "Number of runs to return (default 10)"
        required: [name]
        additionalProperties: false
      timeout_seconds: 10
      env:
        DB_PATH: /var/lib/myapp/jobs.db
```

Scripts receive the LLM's arguments as **JSON on stdin**:

```bash
#!/usr/bin/env bash
INPUT=$(cat)
NAME=$(echo "$INPUT" | jq -r '.name')
LIMIT=$(echo "$INPUT" | jq -r '.limit // 10')
sqlite3 "$DB_PATH" \
  "SELECT * FROM runs WHERE job='$NAME' ORDER BY started DESC LIMIT $LIMIT;"
```

**Field reference:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | ✓ | Tool name shown to LLM. Must match `^[a-z][a-z0-9_]*$`, no collisions with built-in names |
| `description` | ✓ | Description passed to LLM in the tool spec |
| `script` | ✓ | Path to script, relative to the agent directory (e.g. `scripts/foo.sh`) |
| `input` | | JSON Schema for parameters. Must be `type: object` at root. If omitted, tool takes no parameters |
| `timeout_seconds` | | Execution timeout (default 30, max 300) |
| `env` | | Extra environment variables to set when running the script |

Scripts run with `TERM_LLM_AGENT_DIR` and `TERM_LLM_TOOL_NAME` set. Symlinks are resolved and containment-checked—scripts cannot escape the agent directory. No approval prompt is shown; scripts in the agent directory are implicitly trusted.

### Tool Permissions

Control which directories and commands tools can access:

```bash
# Allow read access to specific directories
term-llm chat --tools read,grep --read-dir /home/user/projects

# Allow write access to specific directories
term-llm chat --tools read,write,edit --read-dir . --write-dir ./src

# Allow specific shell commands (glob patterns)
term-llm chat --tools shell --shell-allow "git *" --shell-allow "npm test"
```

When a tool needs access outside approved directories, term-llm prompts for approval with options:
- **Proceed once**: Allow this specific action
- **Proceed always**: Allow for this session (memory only)
- **Proceed always + save**: Allow permanently (saved to config)

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

## Usage Tracking

View token usage and costs from local CLI tools:

```bash
term-llm usage                           # Show all usage
term-llm usage --provider claude-code    # Filter by provider
term-llm usage --provider term-llm       # term-llm usage only
term-llm usage --since 20250101          # From specific date
term-llm usage --breakdown               # Per-model breakdown
term-llm usage --json                    # JSON output
```

Supported sources: Claude Code, Gemini CLI, and term-llm's own usage logs.

## Session Management

Chat sessions are automatically stored locally and can be managed with the `sessions` command. Each session is assigned a sequential number (#1, #2, #3...) for easy reference:

```bash
term-llm sessions                        # List recent sessions
term-llm sessions list --provider anthropic  # Filter by provider
term-llm sessions search "kubernetes"    # Search session content
term-llm sessions show 42                # Show session details (by number)
term-llm sessions show #42               # Same thing (explicit # prefix)
term-llm sessions export 42 [path.md]    # Export as markdown
term-llm sessions name 42 "my session"   # Set custom name
term-llm sessions delete 42              # Delete a session
term-llm sessions reset                  # Delete all sessions
term-llm chat --resume=42                # Resume a session by number
```

Sessions are stored in a SQLite database at `~/.local/share/term-llm/sessions.db`. Configure session storage in your config:

```yaml
sessions:
  enabled: true       # Master switch (default: true)
  max_age_days: 0     # Auto-delete sessions older than N days (0=never)
  max_count: 0        # Keep at most N sessions (0=unlimited)
  path: ""            # Optional DB path override (supports :memory:)
```

Use CLI overrides when needed:

```bash
term-llm chat --no-session                      # Do not read/write session DB
term-llm ask --session-db /tmp/term-llm.db ... # Use an alternate DB path
```

### Conversation Inspector

While in `chat` or `ask` mode, press `Ctrl+O` to open the conversation inspector. This shows the full conversation history including tool calls and results, with vim-style navigation:

| Key | Action |
|-----|--------|
| `j/k` | Scroll up/down |
| `g/G` | Go to top/bottom |
| `e` | Toggle expand/collapse |
| `q` | Close inspector |

Config is stored at `~/.config/term-llm/config.yaml`:

```yaml
default_provider: anthropic

providers:
  # Built-in providers - type is inferred from the key name
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.2
    credentials: codex  # or "api_key" (default)

  xai:
    model: grok-4-1-fast  # grok-4, grok-3, grok-code-fast-1

  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm

  gemini:
    model: gemini-3-flash-preview  # uses GEMINI_API_KEY

  gemini-cli:
    model: gemini-3-flash-preview  # uses ~/.gemini/oauth_creds.json

  zen:
    model: glm-4.7-free
    # api_key is optional - leave empty for free tier

  copilot:
    model: gpt-4.1  # free tier, or gpt-5.2-codex for paid

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
  #   models:  # optional: enable autocomplete for --provider cerebras:<TAB>
  #     - llama-4-scout-17b-16e-instruct
  #     - llama-4-maverick-17b-128e-instruct
  #     - qwen-3-32b

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
  provider: gemini  # gemini, openai, xai, flux, or openrouter
  output_dir: ~/Pictures/term-llm

  gemini:
    api_key: ${GEMINI_API_KEY}
    # model: gemini-2.5-flash-image

  openai:
    api_key: ${OPENAI_API_KEY}
    # model: gpt-image-1

  xai:
    api_key: ${XAI_API_KEY}
    # model: grok-2-image-1212

  flux:
    api_key: ${BFL_API_KEY}
    # model: flux-2-pro

  openrouter:
    api_key: ${OPENROUTER_API_KEY}
    # model: google/gemini-2.5-flash-image

embed:
  # provider: gemini  # gemini, openai, jina, voyage, ollama (auto-detected from default_provider)

  # gemini:
  #   api_key: ${GEMINI_API_KEY}
  #   model: gemini-embedding-001

  # openai:
  #   api_key: ${OPENAI_API_KEY}
  #   model: text-embedding-3-small

  # jina:
  #   api_key: ${JINA_API_KEY}        # free tier: 10M tokens at jina.ai/embeddings
  #   model: jina-embeddings-v3

  # voyage:
  #   api_key: ${VOYAGE_API_KEY}
  #   model: voyage-3.5

  # ollama:
  #   base_url: http://localhost:11434
  #   model: nomic-embed-text

search:
  provider: duckduckgo  # exa, brave, google, or duckduckgo (default)

  # exa:
  #   api_key: ${EXA_API_KEY}

  # brave:
  #   api_key: ${BRAVE_API_KEY}

  # google:
  #   api_key: ${GOOGLE_SEARCH_API_KEY}
  #   cx: ${GOOGLE_SEARCH_CX}

tools:
  max_tool_output_chars: 20000  # truncate tool outputs before sending to LLM (default 20000, 0 to disable)
```

### Per-Command Provider/Model

Each command (exec, ask, edit) can have its own provider and model, overriding the global default:

```yaml
default_provider: anthropic  # global default

providers:
  anthropic:
    model: claude-sonnet-4-6
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
  model: gpt-5.2       # edit uses OpenAI
```

**Precedence** (highest to lowest):
1. CLI flag: `--provider openai:gpt-5.2`
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
term-llm ask --provider anthropic:claude-sonnet-4-6-thinking "complex question"
```

Or in config:
```yaml
providers:
  anthropic:
    model: claude-sonnet-4-6-thinking  # enables 10k token thinking budget
```

Extended thinking allows Claude to reason through complex problems before responding. The thinking process uses ~10,000 tokens and is not shown in the output.

### Web Search

When using `-s`/`--search`, some providers (Anthropic, OpenAI, xAI, Gemini) have native web search built-in. xAI also includes X (Twitter) search. Others use external tools (configurable search provider + [Jina Reader](https://jina.ai/reader/)).

You can force external search even for providers with native support—useful for consistency, debugging, or when native search doesn't work well for your use case.

**CLI flags:**
```bash
term-llm ask "latest news" -s --no-native-search  # Force external search tools
term-llm ask "latest news" -s --native-search     # Force native (override config)
```

**Global config** (applies to all providers):
```yaml
search:
  force_external: true  # Never use native search, always use external tools
```

**Per-provider config:**
```yaml
providers:
  gemini:
    model: gemini-2.5-flash
    use_native_search: false  # Always use external search for this provider

  anthropic:
    model: claude-sonnet-4-6
    # use_native_search: true  # Default: use native if available
```

**Priority** (highest to lowest):
1. CLI flag: `--native-search` or `--no-native-search`
2. Global config: `search.force_external: true`
3. Provider config: `use_native_search: false`
4. Default: use native search if provider supports it

### Search Providers

When using external search (non-native), you can choose from multiple search providers:

| Provider | Environment Variable | Description |
|----------|---------------------|-------------|
| DuckDuckGo (default) | — | Free, no API key required |
| [Exa](https://exa.ai) | `EXA_API_KEY` | AI-native semantic search |
| [Brave](https://brave.com/search/api/) | `BRAVE_API_KEY` | Independent index, privacy-focused |
| [Google](https://developers.google.com/custom-search) | `GOOGLE_SEARCH_API_KEY` + `GOOGLE_SEARCH_CX` | Google Custom Search |

**Configure in `~/.config/term-llm/config.yaml`:**
```yaml
search:
  provider: exa  # exa, brave, google, or duckduckgo (default)

  exa:
    api_key: ${EXA_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}

  google:
    api_key: ${GOOGLE_SEARCH_API_KEY}
    cx: ${GOOGLE_SEARCH_CX}  # Custom Search Engine ID
```

Run `term-llm config` to see which search providers have credentials configured.

### Credentials

Most providers use API keys via environment variables. Some providers use OAuth credentials from companion CLIs:

| Provider | Credentials Source | Description |
|----------|-------------------|-------------|
| `anthropic` | `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, or OAuth | Anthropic API key or OAuth token |
| `openai` | `OPENAI_API_KEY` | OpenAI API key |
| `chatgpt` | `~/.config/term-llm/chatgpt_creds.json` | ChatGPT Plus/Pro OAuth |
| `copilot` | `~/.config/term-llm/copilot_creds.json` | GitHub Copilot OAuth |
| `gemini` | `GEMINI_API_KEY` | Google AI Studio API key |
| `gemini-cli` | `~/.gemini/oauth_creds.json` | gemini-cli OAuth (Google Code Assist) |
| `xai` | `XAI_API_KEY` | xAI API key |
| `openrouter` | `OPENROUTER_API_KEY` | OpenRouter API key |
| `zen` | `ZEN_API_KEY` (optional) | Empty for free tier |

**Anthropic**, **ChatGPT**, **Copilot**, and **Gemini CLI** work without any API key if you have a subscription or the CLI installed and logged in:

```bash
term-llm ask --provider anthropic "question"  # uses OAuth token (runs `claude setup-token` on first use)
term-llm ask --provider chatgpt "question"    # uses ChatGPT Plus/Pro subscription
term-llm ask --provider copilot "question"    # uses GitHub Copilot OAuth
term-llm ask --provider gemini-cli "question" # uses ~/.gemini/oauth_creds.json
```

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
