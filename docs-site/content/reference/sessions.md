---
title: "Session management"
weight: 5
description: "List, search, resume, tag, title, export, and prune local sessions stored in SQLite."
kicker: "State"
featured: true
---
## Session commands

```bash
term-llm sessions
term-llm sessions list --provider anthropic
term-llm sessions search "kubernetes"
term-llm sessions show 42
term-llm sessions export 42
term-llm sessions name 42 "investigate auth flow"
term-llm sessions tag 42 bughunt auth
term-llm sessions untag 42 auth
term-llm sessions autotitle
term-llm sessions autotitle --dry-run
term-llm sessions browse
term-llm sessions gist 42
term-llm sessions delete 42
term-llm sessions reset
term-llm chat --resume=42
```

Sessions are numbered sequentially for convenience, so `42` and `#42` both work.

## Storage

Sessions are stored in SQLite at:

```text
~/.local/share/term-llm/sessions.db
```

That store is not just for interactive `chat` and `ask` runs. LLM jobs also use it by default now, so background runs can leave a persisted transcript and tool trail instead of vanishing into process memory.

Session storage config:

```yaml
sessions:
  enabled: true
  max_age_days: 0
  max_count: 0
  path: ""
```

CLI overrides:

```bash
term-llm chat --no-session
term-llm ask --session-db /tmp/term-llm.db ...
```

## Session titles

Sessions can have titles set in two ways:

- **Manual:** `term-llm sessions name 42 "investigate auth flow"` sets a custom name that always takes priority.
- **Auto-generated:** `term-llm sessions autotitle` uses the configured fast LLM provider to generate short and long titles from the first few messages of each session.

Titles are generated and saved by default. Use `--dry-run` to preview without saving:

```bash
# Generate and save titles for the 50 most recent sessions
term-llm sessions autotitle

# Preview without saving
term-llm sessions autotitle --dry-run

# Regenerate even for sessions that already have titles or custom names
term-llm sessions autotitle --force

# Title sessions older than 10 minutes instead of the default 3
term-llm sessions autotitle --min-age 10m
```

The command is safe to run repeatedly — it skips sessions that already have a generated title or a custom name (unless `--force` is used), and does not contact the LLM provider when there is nothing to do. Sessions updated less than 3 minutes ago are skipped by default (`--min-age 3m`) so the conversation has time to develop before titling.

When displaying sessions (in `list`, `show`, `export`, and `browse`), titles are chosen in priority order:

1. User-set name (from `sessions name`)
2. Generated short/long title (from `sessions autotitle`)
3. Summary (first user message)

## Conversation inspector

While in `chat` or `ask`, press `Ctrl+O` to open the conversation inspector.

| Key | Action |
|---|---|
| `j/k` | Scroll up/down |
| `g/G` | Go to top/bottom |
| `e` | Toggle expand/collapse |
| `q` | Close inspector |

## What sessions are for

Use sessions when you want:

- resumable conversation state
- transcript search
- exported chat history
- per-session naming and tagging
- persisted LLM job transcripts and tool history for background runs

Use [Memory](/guides/memory/) when you want durable facts and behavioral insights that survive beyond one specific chat.

## Related pages

- [Configuration](/reference/configuration/)
- [Memory](/guides/memory/)
