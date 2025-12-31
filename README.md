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

```bash
go install github.com/samsaffron/term-llm@latest
```

Or build from source:

```bash
git clone https://github.com/samsaffron/term-llm
cd term-llm
go build
```

## Setup

On first run, term-llm will prompt you to choose a provider (Anthropic or OpenAI).

Set your API key as an environment variable:

```bash
# For Anthropic
export ANTHROPIC_API_KEY=your-key

# For OpenAI
export OPENAI_API_KEY=your-key
```

## Usage

```bash
term-llm exec "your request here"
```

Use arrow keys to select a command, Enter to execute. Select "something else..." to refine your request.

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

Config is stored at `~/.config/term-llm/config.yaml`:

```yaml
provider: anthropic  # or "openai"

# Custom context added to system prompt
system_context: |
  I use Arch Linux with zsh.
  I prefer ripgrep over grep, fd over find.

anthropic:
  model: claude-sonnet-4-5

openai:
  model: gpt-5.2
```

### Shell Completions

Generate and install shell completions:

```bash
term-llm config completion zsh --install   # Install for zsh
term-llm config completion bash --install  # Install for bash
term-llm config completion fish --install  # Install for fish
```

## License

MIT
