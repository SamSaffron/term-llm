# term-llm

Translate natural language into shell commands using LLMs.

```
$ term-llm "find all go files modified today"

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
term-llm "your request here"
```

Use arrow keys to select a command, Enter to execute. Select "something else..." to refine your request.

## Shell Integration (Recommended)

Commands run by term-llm don't appear in your shell history. To fix this, add a shell function that uses `--print-only` mode.

### Zsh

Add to `~/.zshrc`:

```zsh
tl() {
  local cmd=$(term-llm --print-only "$@")
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
  local cmd=$(term-llm --print-only "$@")
  if [[ -n "$cmd" ]]; then
    history -s "$cmd"  # add to history
    eval "$cmd"
  fi
}
```

Then use `tl` instead of `term-llm`:

```bash
tl "find large files"
```

## Configuration

```bash
term-llm --config show   # Show current config
term-llm --config edit   # Edit config file
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

## License

MIT
