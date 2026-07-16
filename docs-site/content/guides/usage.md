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
term-llm loop @web-researcher --done-file ...  # use web-researcher agent in loop
term-llm ask @shell "find the 3 biggest files"
```

See [Agents](/guides/agents/) for more details on creating and managing agents.

### Using Skills

Skills inject task-specific context into any command without changing the model or provider:

```bash
term-llm ask --skills git "how to squash commits"   # use git skill
term-llm chat --skills git,docker                   # combine multiple skills
term-llm edit --skills refactoring -f main.go "refactor this"
term-llm exec --skills devops "set up a cron job"
```

Use `--skills all` to load every available skill, or `--skills none` to suppress defaults.

Manage skills with the `skills` subcommand:

```bash
term-llm skills                    # list all skills
term-llm skills new my-skill       # create a new skill
term-llm skills show git           # inspect a skill
term-llm skills browse             # browse available skills
```

See [Skills](/guides/skills/) for the full guide on creating, sharing, and extending skills with custom tools.

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

### TUI attachments

In `term-llm chat`, `Ctrl+F` or `/file <path>` attaches a local text file to the next message. Globs are supported by `/file`, and `/file clear` removes pending file attachments. The TUI reads file contents into the prompt as text, rejects binary files, and accepts text files up to 20 MB. Embedded file contents are wrapped in explicit begin/end markers so the model can tell where each attachment starts and ends. Very large text files can still exceed a model's context window or cost more tokens.

Pasting an image from the clipboard attaches it as an image when the terminal/clipboard integration exposes image data. Pasted images use the same 20 MB decoded limit as web/API uploads.

### Chat Slash Commands

| Command | Description |
|---------|-------------|
| `/help` | Show help |
| `/clear` | Clear conversation |
| `/model` | Show current model |
| `/search` | Toggle web search |
| `/fast` | Toggle fast/priority service tier for supported OpenAI/ChatGPT models |
| `/mcp` | Manage MCP servers |
| `/goal` | Set, edit, pause, resume, clear, or show the persistent session goal |
| `/side <question>` | Ask a private, tool-less one-turn question without interrupting or changing the main conversation |
| `/share [new] [public]` | Share the session as a GitHub Gist; repeat to update or create a new gist |
| `/quit` | Exit chat |

When web search is enabled, the chat status line shows `web`; when fast service tier is enabled, it shows `fast`.

### Side questions

`/side <question>` opens an overlay over the current TUI or web conversation and sends immediately. `/side` alone opens or reopens the overlay with its dedicated `Ask a follow-up…` composer focused. The main answer keeps running and remains visible. Each send is an independent one-turn provider request with no local tools, MCP, search, approvals, attachments, model picker, slash commands, queue, or subagents. It uses a fresh snapshot of the latest completed main context plus up to 20 successful side exchanges kept only in the active runtime's memory. Side questions are never added to the transcript, resume state, exports, search index, compaction input, title input, or session message count.

Side exchanges appear chronologically in one compact scrollable transcript (`You` question, rendered Markdown `Side` answer). After an answer completes, the pinned single-line side composer accepts a follow-up with Enter. While answering, the composer is unavailable and `Esc` cancels only the side request while leaving the overlay open. When idle, `Esc` first clears a non-empty side draft, then closes the overlay when the draft is empty. In the TUI, PageUp/PageDown or Ctrl+Up/Ctrl+Down scroll the transcript, Ctrl+C copies the latest answer, and Ctrl+X twice clears history. The web overlay provides equivalent cancel, copy-latest, and confirmed-clear controls and sanitizes rendered Markdown. Main composer draft, session, transcript position, and main cancellation remain independent.

Side history is intentionally lost on a new/cleared/resumed session, runtime eviction, or process/server restart. Promotion or merging a side answer into the main transcript is not supported; copy it or restate the conclusion in the main conversation.

### Persistent goals

Use `/goal` in chat when you want the agent to keep pursuing a durable objective across automatic continuations:

```text
/goal set finish the migration and verify every test
/goal set --budget 50000 finish the migration and verify every test
/goal edit finish the migration, update docs, and verify every test
/goal pause
/goal resume
/goal clear
```

An active goal is stored with the session, so it survives reloads and can continue from the TUI, web UI, or other runner-backed surfaces. While a goal is active, term-llm injects goal-steering prompts between turns and exposes `get_goal`/`update_goal` tools to the model. The model should call `update_goal` only when the objective is genuinely complete or strictly blocked; otherwise the runner continues until the goal is paused, completed, blocked, cancelled, or its optional token budget is exhausted. The status line shows a `goal` chip with token usage when a goal is present.

Changing sessions, compacting, handing over, or switching models pauses the current goal so the next turn does not unexpectedly continue old work.

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | | Override provider, optionally with model (e.g., `openai:gpt-5.2`) |
| `--file` | `-f` | File(s) to include as context (supports globs, line ranges, 'clipboard') |
| `--auto-pick` | `-a` | Auto-execute the best suggestion without prompting (exec only) |
| `--agent` | `-a` | Use a specific agent (ask/chat only; see also `@agent` syntax) |
| `--skills` | | Skills mode: all, none, or comma-separated names |
| `--max N` | `-n N` | Limit to N options in the selection UI |
| `--search` | `-s` | Enable web search and page reading (see [Search](/guides/search/) for providers) |
| `--native-search` | | Use provider's native search (override config) |
| `--no-native-search` | | Force external search tools instead of native |
| `--print-only` | `-p` | Print the command instead of executing it |
| `--debug` | `-d` | Show provider debug information |
| `--debug-raw` | | Emit raw debug logs with timestamps (tool calls/results, raw requests) |
| `--json` | | Emit JSONL event stream on stdout, one event per line (ask only; see below) |
| `--system-message` | `-m` | Custom system message/instructions |
| `--stats` | | Show session statistics (time, tokens, tool calls) |
| `--no-session` | | Disable session persistence for this command |
| `--session-db` | | Override sessions database path (supports `:memory:`) |
| `--max-turns` | | Max agentic turns for tool execution (default: 50 for ask/exec, 200 for chat) |
| Tool concurrency | | When a model emits many parallel tool calls in one turn, term-llm runs at most 20 tool calls concurrently and queues the rest for that turn. |
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
term-llm ask --json "explain git rebase" | jq -c .   # JSONL event stream

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

### JSON event stream (`ask --json`)

`term-llm ask --json` emits a newline-delimited JSON (JSONL) event stream on
stdout, one event per line, for scripting and automation. It implies `--text`
(no rich terminal rendering) and is incompatible with `--debug-raw`. Human
progress and warnings stay on stderr.

Every event shares an envelope:

```json
{"type": "text.delta", "seq": 3, "ts": "2026-04-19T10:30:00.123456789Z", "text": "Hello"}
```

Event types, in typical order:

| Type | Payload |
|------|---------|
| `session.started` | `session_id`, `provider`, `model`, `agent`, `tools`, `mcp`, `yolo`, `search`, `resuming` |
| `text.delta` | `text`, one chunk of streamed response |
| `tool.started` | `call_id`, `name`, `info`, `args` (raw JSON or `null`) |
| `tool.completed` | `call_id`, `name`, `info`, `success` |
| `usage` | `input_tokens`, `output_tokens`, `cached_input_tokens`, `cache_write_tokens` |
| `phase` | `phase` |
| `retry` | `attempt`, `max`, `wait_seconds` |
| `image` | `path` |
| `diff` | `path`, `old`, `new`, `line` |
| `progressive.result` | `exit_reason`, `finalized`, plus optional `session_id`/`sequence`/`reason`/`message`/`progress`/`final_response`/`fallback_text` (only with `--progressive`) |
| `error` | `message` |
| `stats` | `duration_ms`, `llm_ms`, `tool_ms`, token counts, `tool_calls`, `llm_calls` |
| `done` | `tokens` |

The last two events are always `stats` then `done`, even on context cancellation
or errors. `seq` starts at 0 and strictly increments.
