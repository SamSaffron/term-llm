---
title: "Usage"
weight: 1
description: "Core command usage for `exec`, `ask`, `chat`, flags, examples, and agent selection."
kicker: "Core flow"
source_readme_heading: "Usage"
featured: true
next:
  label: File editing
  url: /guides/file-editing/
---
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
term-llm chat @codebase                        # explore a repository
term-llm loop @researcher --done-file ...     # use researcher agent in loop
term-llm ask @shell "find the 3 biggest files"
```

See [Agents](/guides/agents/) for more details on creating and managing agents.

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
| `Left click` | Move cursor in chat input |
| `Shift+drag` | Select/copy chat output text in terminal |

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

# Generate videos (Venice AI)
term-llm video "a corgi surfing at sunset"
term-llm video "make Romeo blink" -i romeo.png
term-llm video "astronaut on mars" --quote-only
```
