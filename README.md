# term-llm

A Swiss Army knife for your terminal—AI-powered commands, answers, and images at your fingertips.

[![Release](https://img.shields.io/github/v/release/samsaffron/term-llm?style=flat-square)](https://github.com/samsaffron/term-llm/releases)

## Features

- **Command suggestions**: Natural language → executable shell commands
- **Ask questions**: Get answers with optional web search
- **File context**: Include files, clipboard, or stdin as context (`-f`)
- **Image generation**: Create and edit images (Gemini, OpenAI, Flux)
- **Multiple providers**: Anthropic, OpenAI, Gemini, Zen (free tier)
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

On first run, term-llm will prompt you to choose a provider (Anthropic, OpenAI, Gemini, or Zen).

### Option 1: Use existing CLI credentials (recommended)

If you have [Claude Code](https://claude.ai/code), [Codex](https://github.com/openai/codex), or [gemini-cli](https://github.com/google-gemini/gemini-cli) installed and logged in, term-llm can use those credentials:

```yaml
# In ~/.config/term-llm/config.yaml
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

# For Gemini
export GEMINI_API_KEY=your-key
```

### Option 3: Use OpenCode Zen (free tier available)

[OpenCode Zen](https://opencode.ai) provides free access to GLM 4.7 and other models. No API key required for free tier, or set `ZEN_API_KEY` for paid models:

```yaml
# In ~/.config/term-llm/config.yaml
provider: zen

zen:
  model: glm-4.7-free  # default model (free)
  # api_key: optional - leave empty for free tier, or set for paid models
```

Or use the `--provider` flag:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
```

## Usage

```bash
term-llm exec "your request here"
```

Use arrow keys to select a command, Enter to execute, or press `h` for detailed help on the highlighted command. Select "something else..." to refine your request.

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | | Override provider (anthropic, openai, gemini, zen) |
| `--file` | `-f` | File(s) to include as context (supports globs, 'clipboard') |
| `--auto-pick` | `-a` | Auto-execute the best suggestion without prompting |
| `--max N` | `-n N` | Limit to N options in the selection UI |
| `--search` | `-s` | Enable web search for current information |
| `--print-only` | `-p` | Print the command instead of executing |
| `--debug` | `-d` | Show full LLM request and response |

### Examples

```bash
term-llm exec "list files by size"              # interactive selection
term-llm exec "compress folder" --auto-pick     # auto-execute best
term-llm exec "find large files" -n 3           # show max 3 options
term-llm exec "install latest node" -s          # with web search
term-llm exec "disk usage" -p                   # print only
term-llm exec --provider zen "git status"       # use specific provider

# With file context
term-llm exec -f error.log "find the cause"     # analyze a file
term-llm exec -f "*.go" "run tests for these"   # glob pattern
git diff | term-llm exec "commit message"       # pipe stdin

# Ask a question
term-llm ask "What is the difference between TCP and UDP?"
term-llm ask "latest node.js version" -s        # with web search
term-llm ask --provider zen "explain docker"    # use specific provider
term-llm ask -f code.go "explain this code"     # with file context
term-llm ask -f clipboard "what is this?"       # from clipboard
cat README.md | term-llm ask "summarize this"   # pipe stdin

# Generate images
term-llm image "a sunset over mountains"
term-llm image "logo design" --provider flux    # use specific provider
term-llm image "make it purple" -i photo.png    # edit existing image
```

## Image Generation

Generate and edit images using AI models from Gemini, OpenAI, or Flux (Black Forest Labs).

```bash
term-llm image "a robot cat on a rainbow"
```

By default, images are:
- Saved to `~/Pictures/term-llm/` with timestamped filenames
- Displayed in terminal via `icat` (if available)
- Copied to clipboard (actual image data, paste-able in apps)

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
provider: anthropic  # anthropic, openai, gemini, or zen

exec:
  suggestions: 3  # number of command suggestions
  instructions: |
    I use Arch Linux with zsh.
    I prefer ripgrep over grep, fd over find.

ask:
  instructions: |
    Be concise. I'm an experienced developer.

anthropic:
  model: claude-sonnet-4-5
  credentials: claude  # or "api_key" (default)

openai:
  model: gpt-5.2
  credentials: codex  # or "api_key" (default)

gemini:
  model: gemini-3-flash-preview
  credentials: gemini-cli  # or "api_key" (default)

zen:
  model: glm-4.7-free
  # api_key is optional - leave empty for free tier

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

### Shell Completions

Generate and install shell completions:

```bash
term-llm config completion zsh --install   # Install for zsh
term-llm config completion bash --install  # Install for bash
term-llm config completion fish --install  # Install for fish
```

## License

MIT
