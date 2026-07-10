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
  strip_image_base64: false
```

By default, image uploads remain portable because session rows keep the image base64 as well as any saved local path. If you prefer a smaller SQLite database and are willing to keep the uploads directory with it, set `sessions.strip_image_base64: true` to store only image path/metadata for image parts that have an `ImagePath`.

CLI overrides:

```bash
term-llm chat --no-session
term-llm ask --session-db /tmp/term-llm.db ...
```

## Worktree-bound sessions

Chat sessions can bind their tools to a git worktree without changing the term-llm process working directory. In the TUI, use `/worktree` (or `/wt`) while in `chat`:

```text
/worktree new [name] [--base REF] [-b branch]
/worktree list
/worktree switch <name-or-dir>
/worktree diff
/worktree merge [--commit] [-m message] [--keep]
/worktree promote <branch>
/worktree root
/worktree rm [name-or-dir] [--force]
```

A bound worktree becomes the session `BaseDir`: relative `read_file`, `write_file`, `edit_file`, `grep`, `glob`, shell working directories, image paths, and spawned agents resolve there. The binding is saved as `worktree_dir` in SQLite and is restored on resume. `/worktree merge` applies worktree changes back to the root checkout staged and uncommitted by default, removes the source worktree, and rebinds the current session to root. The merge success message includes the snapshot commit as a recovery pointer. Use `--keep` to retain the worktree and its session binding after merging. If another session is bound to the worktree, term-llm asks before removing it; failed or conflicting merges always preserve it.

In the Web UI, the header worktree chip is available for draft sessions launched from a git checkout. Choose or create a worktree before the first send; the choice is locked to that session after the first message and sent as `worktree_dir` on the Responses API request. The merge action also removes the worktree by default and asks for confirmation before forcing removal when sessions still use it.

## File change history

When `file_tracking.enabled` is true, term-llm records file changes made by agent tools and exposes them in the web UI as a per-session **Changes** panel. The panel opens automatically on wide screens when the active session changes files; on narrower screens it stays behind the file-changes button so it does not crush the chat column.

```yaml
file_tracking:
  enabled: true
```

Tracked changes include:

- `write_file`, `edit_file`, and `unified_diff` writes
- files created, modified, or deleted by `shell` commands when they are detectable
- cumulative before/after diffs for the session, not just the last tool call

Shell tracking is best with explicit `affected_paths` hints:

```json
{
  "command": "go generate ./...",
  "working_dir": "/path/to/repo",
  "affected_paths": ["**/*.go", "go.mod", "go.sum"]
}
```

Without hints, term-llm falls back to `git status` in repositories and to paths already touched in the session. That catches common repo work, but broad scripts writing outside git need hints if you want reliable history.

The file-change store keeps actual file contents, subject to the configured byte caps. Large files, binary files, and over-budget sessions are still listed as metadata-only changes, but their full diffs are not retained. See [Configuration](/reference/configuration/#file-change-tracking-config/) for retention and privacy details.

## Context compaction

Long sessions do not keep sending the entire transcript forever. When `auto_compact` is enabled (the default) and term-llm knows the model's input limit, the engine tracks an estimated prompt size and compacts before the active context would grow too large.

Compaction is intentionally non-destructive:

1. The full original transcript remains in the SQLite session store for scrollback and auditability.
2. term-llm asks the model for an internal continuation summary of the old context.
3. It appends a compacted active-context block to the same session: a `[Context Compaction]` summary message followed by a recent raw tail of exact messages.
4. The session records `compaction_seq` as the sequence number where active model context now begins, plus a `compaction_count`.
5. Future model requests load only messages at or after that boundary, plus the configured system/instruction prompt when needed.

The recent raw tail is duplicated on purpose. The original copy remains visible in the transcript where it happened; the appended copy gives the model exact recent wording, tool calls, and tool results after the summary. To avoid confusing UI echo, appended retained-tail rows are marked `compaction_tail` in storage. TUI and Web renderers suppress those rows, while the active model-context loader still sends them to the provider.

Practical consequences:

- You can still scroll/search the pre-compaction transcript; old history is not deleted.
- The visible compaction marker shows where the active context was reset.
- The hidden retained tail does not count as a visible message and is skipped by search/result continuation IDs, but it remains part of the active LLM context.
- Resuming a compacted session starts from `compaction_seq` rather than replaying the whole transcript.
- Older sessions compacted before `compaction_tail` existed are handled best-effort by matching the post-summary duplicate tail against the pre-summary transcript.

You can disable automatic compaction globally:

```yaml
auto_compact: false
```

When disabled, sessions still persist normally, but term-llm will not automatically rewrite the active context to stay under known model limits.

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

The command is safe to run repeatedly. It skips sessions that already have a generated title or a custom name (unless `--force` is used), and does not contact the LLM provider when there is nothing to do. Sessions updated less than 3 minutes ago are skipped by default (`--min-age 3m`) so the conversation has time to develop before titling.

When displaying sessions (in `list`, `show`, `export`, and `browse`), titles are chosen in priority order:

1. User-set name (from `sessions name`)
2. Generated short/long title (from `sessions autotitle`)
3. Summary (first user message)

## Conversation inspector

While in `chat` or `ask`, press `Ctrl+O` to open the conversation inspector. The inspector is intended as a debug view of the persisted conversation context. For compacted sessions it shows `Context compaction` boundary blocks; press `e` to expand all hidden inspector details, including full internal compaction summaries, previous-turns excerpts, and retained raw tail rows that remain in active model context but are hidden from normal chat rendering.

| Key | Action |
|---|---|
| `j/k` | Scroll up/down |
| `g/G` | Go to top/bottom |
| `e` | Expand all hidden inspector details |
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
