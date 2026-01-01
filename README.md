# term-llm

Translate natural language into shell commands using LLMs.

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

On first run, term-llm will prompt you to choose a provider (Anthropic, OpenAI, or Gemini).

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

## Usage

```bash
term-llm exec "your request here"
```

Use arrow keys to select a command, Enter to execute, or press `h` for detailed help on the highlighted command. Select "something else..." to refine your request.

### Flags

| Flag | Short | Description |
|------|-------|-------------|
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

# Ask a question
term-llm ask "What is the difference between TCP and UDP?"
term-llm ask "latest node.js version" -s       # with web search
```

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
provider: anthropic  # or "openai" or "gemini"

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
```

### Credentials

Each provider supports a `credentials` field:

| Value | Description |
|-------|-------------|
| `api_key` | Use environment variable (default) |
| `claude` | Use Claude Code credentials (Anthropic) |
| `codex` | Use Codex CLI credentials (OpenAI) |
| `gemini-cli` | Use gemini-cli OAuth credentials (Gemini) |

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
