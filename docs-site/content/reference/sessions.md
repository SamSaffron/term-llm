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

## Conversation paths

Use `/tree` in the TUI, or the per-turn branch actions in the Web UI, to continue from an earlier message without deleting the current conversation. Every path is stored as a normal resumable child session; the tree view can switch between all surviving alternatives.

When starting a path, choose what it should retain from the later turns being left out:

- **Start clean** copies only the conversation prefix through the selected branch point.
- **Bring useful context** asks the current model for short, non-authoritative notes covering useful findings, attempts, test results, and files touched.
- **Bring specific context…** adds your focus instructions to that same bounded note-generation policy.

Path notes are inserted as internal developer context before the first new user turn and shown as an expandable/inspectable artifact. They do not count as user messages. Generation uses a one-turn ephemeral helper request with bounded input and output. In the TUI, the child path opens immediately and note creation appears as live transcript work; you can draft at once, and a message submitted before the notes finish is queued until they are inserted. If generation is cancelled or fails, the queued message is restored to the composer and the new path remains clean. The Web flow prepares the selected context as part of its branch request.

Conversation branching rewinds model context only. Filesystem changes, commands, network requests, and other tool side effects from the previous path are not undone.

## Storage

Sessions are stored in SQLite at:

```text
~/.local/share/term-llm/sessions.db
```

LLM jobs use the same store by default, preserving their transcripts and tool history.

Session storage config:

```yaml
sessions:
  enabled: true
  max_age_days: 0
  max_count: 0
  path: ""
  strip_image_base64: false
```

By default, session rows keep base64-encoded image data and any saved local path, so uploads remain portable. To reduce the database size, set `sessions.strip_image_base64: true`; image parts with an `ImagePath` will retain only their path and metadata.

CLI overrides:

```bash
term-llm chat --no-session
term-llm ask --session-db /tmp/term-llm.db ...
```

## Web projects and immutable session bindings

With project mode enabled, the Web sidebar groups conversations by durable project and keeps unbound conversations under **No project**. On the first project-enabled startup, term-llm registers the canonical startup directory as a bootstrap project unless the registry already contains records. Git startup paths normalize to the main repository root; non-Git paths remain exact. Migration 47 adds nullable `project_id` metadata without changing existing `cwd` or `worktree_dir` snapshots. Only historical sessions that unambiguously match the bootstrap root are backfilled; all others remain under **No project**.

A project's ID is stable even if the project is renamed, archived, or restored. A session's `project_id` is grouping and provenance, while `cwd` and `worktree_dir` are its authoritative immutable execution snapshot. Existing project conversations may resume after archival, but archived projects cannot start new conversations. Missing paths, replaced symlinks, moved roots, and cross-project worktrees fail closed before a model or tool run. A missing managed worktree may fall back only to its owning project's validated canonical root.

In the Web UI, **New chat** opens a single draft surface with a project picker. It defaults to the last project context and includes **No project**, which uses the server startup directory without project provenance. Each project can still keep one Hub-node-scoped local composer draft behind that surface. Its unsent prompt, provider/model/effort/reasoning choices, and selected managed worktree survive reloads independently; attachments remain isolated in memory for the current tab. Drafts are client-only until first send, when the server's immutable project/worktree snapshot becomes authoritative. Archiving a project disables its unsent draft but does not block its existing conversations.

Eligible **No project** conversations can be assigned once through the dedicated action. Assignment proves that a Git CWD/worktree belongs to the same main repository, or that a non-Git CWD exactly matches, and writes only `project_id`; it never changes execution paths.

Project registration and selection do not grant filesystem or shell permission. Workspace confirmations, configured read/write directories, shell approvals, and Guardian remain separate and authoritative.

## Worktree-bound sessions

Chat sessions can bind their tools to a git worktree without changing the term-llm process working directory. In the TUI, use `/worktree` (or `/wt`) while in `chat`:

```text
/worktree new [name] [--clean] [--base REF] [-b branch]
/worktree browse
/worktree switch <name-or-dir>
/worktree diff
/worktree promote [--branch]
/worktree root
/worktree rm [name-or-dir] [--force]
```

By default, `/worktree new` transfers staged, unstaged, and untracked changes from the root checkout into the new worktree. Add `--clean` to create the new worktree from the selected base without transferring those changes; the existing work in progress remains in the root checkout.

A bound worktree becomes the session `BaseDir`: relative `read_file`, `write_file`, `edit_file`, `grep`, `glob`, shell working directories, image paths, and spawned agents resolve there. The binding is saved as `worktree_dir` in SQLite and is restored on resume. For file/path tools it is only a proposed primary workspace: the first access asks the human to allow session-scoped read/write for the whole canonical worktree and states that shell remains separately controlled. A confirmed binding resumes without another prompt. Switching worktrees invalidates a mismatched primary confirmation while preserving additional workspace grants; shell approval remains independent throughout.

`/worktree promote` always promotes the current bound worktree; it does not accept a worktree name or path. The root checkout must be clean. By default, promotion applies the worktree changes onto the branch currently checked out in root, leaves them staged and uncommitted, removes the source worktree when no other session is using it, and rebinds the session to root. If applying the changes conflicts, term-llm preserves the source worktree and offers assisted recovery. When confirmed, it rebinds the session to root, takes a fresh snapshot so changes made while the prompt was open are included, applies that snapshot directly on the current root branch, and asks the LLM to resolve conflicts there.

Use `/worktree promote --branch` to avoid applying onto the current root branch. This mode creates and checks out a new root branch named after the managed worktree at the worktree HEAD, applies dirty and untracked changes there as staged and uncommitted changes, rebinds the session to root, and leaves the original worktree in place. To promote another worktree in either mode, switch to it first with `/worktree switch` or select it in `/worktree browse`.

In the Web UI, the header worktree chip is scoped to the active project draft or persisted conversation. Choose or create a managed worktree before the first send; `project_id` and optional `worktree_dir` are validated together and then locked into the session. Managed worktrees may live under term-llm's XDG data directory rather than inside the project path, so ownership is checked by main-repository identity. The accessible worktree sheet provides diff, merge, promote, and removal actions without browser prompt dialogs. In `--no-projects` mode, the temporary legacy worktree routes and startup-repository behavior remain available for compatibility.

## File change history

When `file_tracking.enabled` is true, term-llm records file changes made by agent tools and exposes them in the web UI as a per-session **Changes** panel. Changed sessions expose a Changes button; on narrower screens the panel opens as a drawer so it does not crush the chat column.

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
