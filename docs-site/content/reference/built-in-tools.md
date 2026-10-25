---
title: "Built-in tools"
weight: 1
description: "The built-in tool surface available to term-llm without MCP."
kicker: "Tooling"
source_readme_heading: "Built-in Tools"
next:
  label: Configuration
  url: /reference/configuration/
---
term-llm includes built-in tools for file operations and shell access. Enable them with the `--tools` flag:

```bash
term-llm chat --tools read_file,shell,grep        # Enable specific tools
term-llm exec --tools read_file,write_file,edit_file,shell,grep,glob,view_image
```

### Available Tools

<!-- BEGIN GENERATED: tools -->
| Tool | Description | Permission policy | Default enabled | Aliases |
|---|---|---|---|---|
| `read_file` | Read file contents. | `read` | true | — |
| `write_file` | Create or overwrite a file with the specified content. | `edit` | true | — |
| `edit_file` | Edit a file. | `edit` | true | — |
| `unified_diff` | Apply file edits using unified diff format. | `edit` | true | — |
| `shell` | Execute a shell command. | `execute` | true | — |
| `grep` | Search file contents using regex patterns (RE2 syntax). | `search` | true | — |
| `glob` | Find files by glob pattern (supports ** for recursive matching). | `search` | true | — |
| `view_image` | View and analyze an image file. | `read` | true | — |
| `show_media` | Register an existing image, video, or audio so it can be placed in your response. | `image` | true | `show_image` |
| `image_generate` | Generate an image from a text prompt. | `image` | true | — |
| `ask_user` | Present questions to the user and gather their responses. | `interactive` | true | — |
| `spawn_agent` | Spawn a sub-agent to handle a specific task autonomously. | `agent` | true | — |
| `wait_agent` | Wait for agents without cancelling them; returns their current status. | `agent` | true | — |
| `continue_agent` | Steer a running agent or resume a stopped agent with a fresh turn budget. | `agent` | true | — |
| `cancel_agent` | Cancel an agent and its active descendants; it may be resumed later. | `agent` | true | — |
| `list_agents` | List agents belonging to this parent session, including interrupted agents from earlier processes. | `agent` | true | — |
| `queue_agent` | Spawn a sub-agent as a background jobs-v2 LLM job and return immediately. | `agent` | true | — |
| `wait_for_jobs` | Wait for one or more queued jobs-v2 agent jobs to finish and return their results. | `agent` | true | — |
| `run_agent_script` | Execute a script bundled with the current agent. | `execute` | true | — |
| `initiate_handover` | Initiate a handover to another agent. | `agent` | true | — |
| `hub_delegate` | Delegate a task to another term-llm node via the Hub. | `agent` | true | — |
| `hub_check_delegation` | Check (and by default wait for) hub delegations started with hub_delegate, returning their status and final responses. | `agent` | true | — |
| `manage_workspace` | Manage session-scoped local workspaces. | `session_state` | false | — |
| `update_plan` | Publish the complete current execution plan. | `session_state` | false | — |
| `live_settings` | Inspect this term-llm live call's model, current voice and supported voices. | `session_state` | false | — |
| `ui_get_source` | Inspect stock web UI source on demand. | `read` | false | — |
| `ui_extensions` | Inspect or reload the running web process's extension directory, enabled order and config/CLI provenance. | `session_state` | false | — |
| `ui_activate_extensions` | Activate completed web extension edits: rescan the configured enabled list and request automatic browser reload when responses finish and drafts are clear. | `session_state` | false | — |
| `show_image` | Display an image to the user via terminal (icat). | `image` | false | — |
| `activate_skill` | Activate a skill to load its specialized instructions and capabilities. | `skill` | false | — |
| `search_skills` | Search for skills by keyword. | `skill` | false | — |
| `live_new_session` | Start a brand-new, empty conversation and bind this live voice call to it. | `session_state` | false | — |
| `live_switch_session` | Bind the current live voice call to a different existing chat session. | `session_state` | false | — |
| `session_directory` | List or search this term-llm instance's chat sessions. | `read` | false | — |
| `create_goal` | Create or replace the persistent objective for this session. | `session_state` | false | — |
| `update_goal` | Mark the persistent session goal complete or blocked after satisfying the required audit. | `session_state` | false | — |
| `get_goal` | Get the persistent objective and budget state for this session. | `read` | false | — |
| `update_progress` | Persist the current best-so-far state as a JSON object without ending the run. | `session_state` | false | — |
| `finalize_progress` | Persist the final best-so-far state as a JSON object and finish the run. | `session_state` | false | — |
<!-- END GENERATED: tools -->

The `shell` tool's `affected_paths` only bounds file-change inspection; pre-execution `output_claims` declares intended transforms, generated deliverables, or materializations. `view_image` returns image content to a vision-capable model, or text from the routed model configured by `vision_via`. `manage_workspace` is automatically added whenever a local path-capable file, search, or image tool is enabled. See the detailed sections below for permission and routing behavior.


Lifecycle tools are enabled together whenever `spawn_agent` is enabled. `spawn.max_depth` is how many levels below this agent it may spawn (`0` or unset uses the default 2), capped by its caller's remaining budget minus one level. Isolated skill children are not agent spawns and remain available at zero budget; their inherited remaining budget is `max(parent_remaining - 1, 0)`, so they cannot spawn agents at zero. A top-level developer with `max_depth: 1` may spawn one child; if called by a parent with two levels remaining, it may still spawn one child, but that child cannot spawn again. Resuming or relaunching a child recalculates the cap from the resuming parent. `spawn_agent` returns `queued` when waiting for a parallel slot, `running` when its wait budget expires, or `completed` when it finishes in time; in every case `agent_id` is the child session ID. There is **no execution deadline** for a spawned child, including in the queue, during a detached run, during delivery, or in a sessionless host (which waits synchronously instead of orphaning a child). The turn limit is a child's only execution budget: the agent's `max_turns` if set, otherwise 500; `continue_agent` grants the same allotment again. `cwd` defaults to the parent's current directory, accepts absolute paths, and resolves relative paths against the parent's current directory; an explicit path must already be a directory. It does not bypass workspace confirmation, permissions, or the parent's approval scope, and the resolved directory persists across `continue_agent`. `wait_agent` and `list_agents` are scoped to the parent session, including after reloading it. `turn_limit`, `cancelled`, and `interrupted` agents can be resumed with `continue_agent`; steering a running child may be reported as `undelivered` when it finishes before consuming the instruction. A turn that ends normally leaves its detached children running so a later turn can collect them. Stopping a turn (the stop button, a steering interrupt, Esc in chat, or a cancelled job) interrupts every running child of that session; they end `interrupted` and stay resumable. On exit, one-shot `ask` waits for outstanding children (Ctrl-C interrupts them); chat and serve interrupt them and leave them resumable. Media produced by a detached child is retained for the `wait_agent` or `continue_agent` result that collects it, including after a process restart. Detached approvals continue through the host's approval UI when available; headless hosts without a prompt transport deny operations rather than waiting silently.

A `failed` child can also be explicitly continued on the **same `agent_id`** when it has saved assistant or tool history. Initialization failures with no such durable transcript are not resumable; spawn a new child instead. Continuation uses the saved transcript and retains completed tool results rather than executing those calls again. Native Claude CLI resume state is saved on acknowledged API-error results as well as successful child runs. Unacknowledged process or transport failures discard native resume state without clearing saved tool results or history; continuation then falls back to the saved conversation history in a fresh native session. This does not automatically retry a failed provider stream, and a model may still choose to request new actions: verify uncertain side effects before asking it to retry them. Parent ownership, depth, active-run concurrency checks, and host lifecycle/recovery restrictions still apply.

For persisted web and terminal-chat sessions, the host automatically resumes the **parent agent loop** when all its direct subagents have stopped and at least one result remains uncollected and unnotified. This is a failsafe for a parent that ends its turn without waiting: it does not require `notify_when_done`, and an agent cannot disable it with a tool argument. Children also receive this lifecycle continuation when their own subagents finish, before reporting their final result to their parent. The host waits for an idle parent and coalesces results; collecting every result with `wait_agent` suppresses an unnecessary wake. A lifecycle turn that ignores the notice is acknowledged, rather than repeatedly awakened for the same child execution. Terminal chat retains wake ownership when you switch sessions and executes the continuation in the background without consuming your composer draft.

`notify_when_done: true` remains an additional **web-only** option for an eager per-child wake while siblings are still running. Neither mode is a raw child notification to the user or a jobs-v2 run. The parent receives a marked internal lifecycle event containing only child IDs, agent names, execution generations, statuses, reasons, and the persisted `auto_resume_on_restart` boolean, with its normal model, tools, and permissions. Child output is never embedded in developer/system authority: the parent uses `wait_agent` to collect untrusted results and media, then may continue the workflow, synthesize a response, or do nothing. The event uses a provider-visible synthetic user turn with durable host provenance, not human-authored speech.

A human may opt a specific child into host-restart continuation with `auto_resume_on_restart: true` on `spawn_agent`, or set/revoke it on a stopped child with the same optional field on `continue_agent`. It defaults off and is persisted on that child across run generations. This is not automatic execution: after a trusted lifecycle notice for the exact generation, the parent may collect state and continue that same child only when the persisted flag is true and the previous owner is proven dead. A restart opt-in never authorizes `cancel_agent`/explicit cancellation, `parent_stopped`, generic cancellation, failure, unknown causes, a live/foreign owner, or bypasses normal approvals and ownership rules. A lifecycle notice cannot change the policy or force a possibly live child. For an opted-in restart, the parent should mention the restart/resumption in its checkpoint message. Parent inactivity timeout or lease loss is not a deadline for independently owned detached children. Graceful shutdown/reload remains an interruption, not a user stop. A nested child that exhausts its ordinary turn allowance still gets a restricted lifecycle allowance to collect/report results, without permission to spawn or continue more children. Telegram, one-shot `ask`, and jobs-v2 `queue_agent` do not provide this unconditional parent-loop wake.

For `term-llm ask`, staged stdin or `-f` text/binary data transiently enables `read_file`; PNG/JPEG/GIF/WebP sources transiently enable both `read_file` and `view_image`. These additions are merged with explicit session tools and do not rewrite the saved tool list. The staged attachment receives an exact-file grant—nearby files, paths mentioned only in text, and symlink escapes receive no automatic access and remain subject to normal approval policy.

### Intent-based grep reranking

When a [classification provider](/guides/classify/) is usable, `grep` accepts an optional `intent` string describing what the agent is looking for (for example, "where session DB writes happen"). Matches are then scored for relevance, incidental matches are dropped, and the rest are returned best first.

- **When it appears:** the `intent` parameter and a short sentence in the tool description are present only if `tools.grep_rerank.enabled` is true (the default), the selected classify provider (`tools.grep_rerank.provider`, else `classify.default_provider`) exists, and its credentials are configured. Otherwise `grep` is unchanged and `intent` is not offered. Credentials are only resolved on the first `intent` call.
- **When it applies:** `intent` is non-blank, `files_with_matches` is not used, and `max_results` is at most `max_candidates`. Without `intent`, behavior and output are identical to before.
- **What is sent:** the intent, the pattern, each candidate match with its surrounding context (bounded to 12 lines / 1200 characters), and its path relative to the search root go to the classify provider. See the [privacy guidance](/guides/classify/#privacy).
- **How it works:** up to `max_candidates` (default 150) matches are searched in one pass and scored in parallel batches of at most 50, with at most four classify calls in flight process-wide. Results scoring at or above `threshold` (default 0.5) are shown in rank order, trimmed to `max_results`. If nothing clears the threshold, the top 3 are shown and the output says so. Fewer than two candidates are returned normally without a classify call.
- **Notes:** the output starts with a one-line `[intent rerank: …]` note with counts, and tells you when the candidate search was capped.
- **Fallback:** all added work shares a hard budget of `timeout_ms` (default 2000 ms). The clock starts when the ordinary `max_results` prefix is found, or when the search completes, whichever comes first. Extra searching, request construction, classification (including transport retries), and ranked rendering share the remaining budget. On any error, timeout, malformed response, or repeated failures (three in a row pause reranking for 60 seconds), `grep` returns the first ordinary prefix in normal order with a one-line `[intent rerank skipped (…); showing unranked results]` note. Intent fallback does not rerun multiline context enrichment. The note never includes provider error text.
- **Worker limits:** at most four intent search pipelines may run process-wide, independently of the four-call classification limit. Admission never waits: saturation returns ordinary grep output with `[intent rerank skipped (busy); showing unranked results]`. A timed-out worker keeps its slot until it actually exits. Intent-only Go scans skip non-regular files; ordinary grep is unchanged.
- **Disable:** set `tools.grep_rerank.enabled: false`.

### Presenting existing media

Use `show_media` when the user should see or listen to a local artifact, rather
than when the model only needs to inspect it. It accepts `path` and an optional
`caption`, subject to the normal read/workspace approval policy. Registration
returns an opaque URL, not a claim that playback succeeded. Place the exact URL
on its own line using **Markdown image syntax for every kind**, including audio:

```markdown
![Listen to the narrated summary](term-llm-media://<reference-returned-by-show_media>)
```

| Kind | Supported formats | Maximum file size |
|------|-------------------|-------------------|
| Image | PNG, JPEG, GIF, WebP, BMP | 25 MiB |
| Video | MP4, WebM | 256 MiB |
| Audio | MP3, PCM/IEEE-float WAV (including valid extensible headers), Ogg Opus/Vorbis, FLAC, M4A/M4B or audio MP4, audio-only WebM | 100 MiB |

Types are inspected from bytes, not guessed from filenames. MP4/WebM audio
classification uses bounded track metadata inspection. Generic containers without
complete metadata within the inspection budget—including MP4/WebM files whose
track metadata lies beyond the bounded inspection window—retain their legacy
video classification and video limit by design. M4A/M4B brands can still identify
audio without available track metadata; inspected video tracks take precedence.
Prefer MP3, faststart M4A (AAC, with metadata at the start), or PCM WAV for
narration. Streamed WAV with unknown RIFF/final data sizes is supported. Raw AAC/ADTS, arbitrary Ogg video/Matroska, RF64/RIFX, and compressed
WAV encodings are not supported. Container recognition does not guarantee codec
playback in every browser; downloads remain useful when a codec is unavailable.

Terminals display audio as a **♪ Audio** file link with the real filename,
contextual label, and a visible `file://` URI. Videos are also file links; there
is no terminal playback command or invented duration. Images retain rich terminal
rendering where supported. Web serves import an approved durable copy, so the
browser never receives a source filesystem path.

Telegram has a separate **50 MiB** upload cap. MP3/M4A use native audio uploads;
confirmed Ogg Opus uses voice uploads. Ogg Vorbis, WAV, FLAC, and audio WebM are
sent as documents. Failed native audio/voice/video uploads retry as documents.
There is no transcoding or automatic audio input to the model. Explicit
`show_image` configurations remain compatible and image-only.

### Indirect image understanding for text-only models

For a text-only model that supports tool calls, add `vision_via` to that model's provider entry:

```yaml
providers:
  local-text:
    type: openai_compatible
    base_url: http://127.0.0.1:1234/v1
    model: qwen-text
    vision_via: gemini
```

Set `vision_via` either at provider level, as above, or on a specific `models:` object when only one model should use the route or needs a different vision backend. Use `provider` to select that provider's default model, or `provider:model` to force a specific model. It inserts a prompt reference such as `[User uploaded image: /.../uploads/image_123.png — use view_image ...]`, auto-enables `view_image`, and lets the model call `view_image` with `file_path` plus an optional `question`. The tool then forwards the processed image to the configured vision-capable provider/model and returns a textual analysis.

Limitations: the primary model must call tools; the `vision_via` provider must be configured and able to process image parts; and `view_image` can only read uploaded images or paths allowed through normal read permissions/approvals.

### Filesystem change attribution

[File change tracking](/reference/sessions/#file-change-history) is enabled by default. When enabled, trusted direct write tools (`write_file`, `edit_file`, `unified_diff`) record witnessed transitions automatically. Shell detection is different: `affected_paths` only bounds pre/post inspection and never creates attribution. Declare intended task output before execution with `output_claims`:

```json
{
  "command": "gofmt -w ./internal/example.go && go generate ./cmd/...",
  "working_dir": "/path/to/project",
  "affected_paths": ["internal/example.go", "cmd/**"],
  "output_claims": [
    {"path": "internal/example.go", "kind": "transform"},
    {"path": "cmd/generated/**", "kind": "generate"}
  ]
}
```

`transform` is compatible with modification/deletion, `generate` with creation/modification/deletion of deliberate deliverables, and `materialize` with clone, checkout, installation, download, extraction, or initial copy output. Materializations and unclaimed effects are shown separately and never contribute agent line totals. Split materialization and later adaptation into two calls when a meaningful adaptation diff is required.

Paths may be literals or doublestar globs, relative to `working_dir` or absolute. Claims implicitly join inspection scope. Git cleanliness, ignore state, prior session paths, and path specificity can help detection but cannot create attribution. Tracking is bounded and best-effort; incomplete coverage and unconfirmed claims are surfaced as diagnostics without changing the shell command result.

### Custom Tools

Agents can declare named, schema-bearing tools backed by shell scripts in the agent directory. These appear to the LLM as first-class tools with their own descriptions and typed parameters. No more asking the LLM to invoke `run_agent_script` with a magic filename.

```yaml
tools:
  enabled: [read_file, shell]
  custom:
    - name: job_status
      description: "List all registered jobs and their last run result."
      script: scripts/job-status.sh
      call: json

    - name: job_run
      description: "Trigger a scheduled job to run immediately."
      script: scripts/job-run.sh
      call: json
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
      script: scripts/job-history.py
      call: json
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

Put the declarations above in the agent's `agent.yaml`, not the global config. With `call: json`, scripts receive the LLM's arguments as **JSON on stdin**. For example, make `scripts/job-history.py` executable and parameterize SQL rather than interpolating model-supplied values:

```python
#!/usr/bin/env python3
import json
import os
import sqlite3
import sys

args = json.load(sys.stdin)
limit = max(1, min(int(args.get("limit", 10)), 100))
with sqlite3.connect(os.environ["DB_PATH"]) as db:
    db.row_factory = sqlite3.Row
    rows = db.execute(
        "SELECT * FROM runs WHERE job = ? ORDER BY started DESC LIMIT ?",
        (args["name"], limit),
    )
    print(json.dumps([dict(row) for row in rows]))
```

Custom tools run from the session working directory when the session is bound to a workspace. In unbound remote sessions, such as a dedicated `--no-projects` Web UI, they run from their agent directory instead of inheriting the server process's working directory. This keeps agent-contained tools usable without weakening the ambient-CWD protection for general shell and file tools.

**Field reference:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | ✓ | Tool name shown to LLM. Must match `^[a-z][a-z0-9_]*$`, no collisions with built-in names |
| `description` | ✓ | Description passed to LLM in the tool spec |
| `script` | ✓ | Path to script, relative to the agent directory (e.g. `scripts/foo.sh`) |
| `call` | | Argument passing mode: `args` (default) passes named flags (`--key value`); `positional` passes positional values; `json` sends JSON on stdin |
| `input` | | JSON Schema for parameters. Must be `type: object` at root. If omitted, tool takes no parameters |
| `timeout_seconds` | | Execution timeout in seconds (default 30, max 3600; an earlier parent deadline or cancellation still applies) |
| `env` | | Extra environment variables to set when running the script |

Scripts run with `TERM_LLM_AGENT_DIR` and `TERM_LLM_TOOL_NAME` set. Symlinks are resolved and containment-checked. Scripts cannot escape the agent directory. No approval prompt is shown; scripts in the agent directory are implicitly trusted.

### Tool Permissions

Control which directories and commands tools can access:

```bash
# Allow read access to specific directories
term-llm chat --tools read_file,grep --read-dir /home/user/projects

# Allow write access to specific directories
term-llm chat --tools read_file,write_file,edit_file --read-dir . --write-dir ./src

# Allow specific shell commands (glob patterns)
term-llm chat --tools shell --shell-allow "git *" --shell-allow "npm test"
```

Shell allowlists are matched command-by-command and word-by-word. A final standalone `*` allows any remaining arguments, while `*` inside an argument does not cross `/`; use `**` for recursive path segments. Every command in a compound expression must be covered by an allowlist pattern.

### Session workspaces

A local launch records its canonical runtime/project directory as the session's **proposed primary workspace**. Launch location alone grants no file authority. When `term-llm chat` has a registered local file, search, or image tool, it asks for workspace access immediately at startup; other interactive surfaces retain first-access confirmation. The choices are **Always allow** (remember this workspace for future sessions), **Allow this session**, or **Deny**, with one shared note that shell commands still require separate approval. Dismissing the proactive startup prompt with Esc/Ctrl+C leaves the decision open and asks again only if a path tool is later used; explicit **Deny** remains latched for the session. A session-only allow is persisted with SQLite-backed sessions, so resume does not reprompt. A remembered allow records the exact canonical root in `$XDG_CONFIG_HOME/term-llm/remembered-workspaces.yaml` (falling back to `~/.config`) and automatically applies to future sessions that propose the same root. When that root is a Git repository's main worktree, its exact linked worktrees reported by `git worktree list` also inherit the approval; arbitrary nested directories and unregistered worktree metadata do not. To revoke a remembered approval, remove that root's entry from this YAML file. A denial blocks workspace path access. Without a human confirmation transport or a matching remembered approval, non-yolo access fails closed. Yolo bypasses confirmation without invoking the prompt or persisting workspace authority; if the session later leaves yolo while the primary is still unconfirmed and unremembered, the next access asks normally.

Sibling directories are never included. Switching a session to another Git worktree replaces the proposal and invalidates a mismatched primary confirmation, except that once the repository's main worktree is approved, switches among its registered worktrees inherit that confirmation for the session. Approving only a linked worktree does not approve the main or sibling worktrees. Other workspace switches require confirmation on first access without removing separately granted references.

Additional reference repositories or directories use the automatically registered `manage_workspace` tool:

- `grant` resolves symlinks, rejects missing/non-directory targets, and narrows a path inside Git to that enclosing repository's own worktree root. It defaults to `read`; `write` is an explicit elevation and implies read.
- `list` distinguishes a proposed or human-confirmed primary workspace from additional workspaces and reports access, provenance, status, and stable IDs.
- `revoke` removes an additional grant by ID or exact canonical path. The model cannot confirm or revoke the primary; only direct human confirmation or session rebinding controls it.

In auto mode, every new additional grant receives one Guardian review using the trusted transcript, requested access, reason, canonical path, existing grants, and session scope. A duplicate or weaker grant is idempotent; elevating read to write receives a separate review. Prompt uses the existing approval transport. Explicit yolo may install an in-memory grant or write-elevation overlay, but never persists it; leaving yolo immediately removes every yolo-created grant and restores any durable read baseline beneath an elevation. Re-entering yolo does not resurrect an earlier overlay. Denials and review failures install nothing and follow the same fail-closed error contract as other Guardian actions.

The root session owns primary confirmation and the additional grant set. Child agents share them immediately, so concurrent parent/child first access produces one primary confirmation prompt and yolo-overlay removal is visible throughout the tree. SQLite-backed sessions persist confirmed primary and non-yolo additional grants across resume; conversation branches copy only those durable grants using the same runtime-setting inheritance as `cwd`, tools, and approval mode. Resume deletes legacy persisted rows marked with yolo provenance on a best-effort basis, and branches never copy them. Custom stores without the optional capability continue to work with runtime-only non-yolo confirmations/grants.

A workspace capability applies only to first-party local file/path tools. It never adds shell patterns or exact commands, never writes project/global approval files, and does not authorize MCP, network, or service actions. Explicit `--read-dir` and `--write-dir` values remain additive and separate.

Serve/web does **not** derive even a proposed workspace from the daemon process CWD. An explicit session/request binding, such as a selected worktree, becomes a pending proposal and uses the same first-access human confirmation in the web approval modal outside yolo. Until such a binding exists, relative local-tool paths and shell commands without an absolute `working_dir` fail closed instead of resolving or executing in daemon CWD; absolute paths remain subject to normal approval, and binding a session restores relative-path behavior. Synchronous/headless non-yolo requests without the workspace-confirmation transport fail closed; explicit yolo bypasses confirmation without creating primary authority. `manage_workspace` is still automatically available when any path-capable local tool is enabled, including an explicit tool list, and is absent when there are no such tools.

When a tool needs access outside approved workspaces/directories, term-llm prompts for approval with options:
- **Proceed once**: Allow this specific action
- **Proceed always**: Allow for this session (memory only)
- **Proceed always + save**: Allow permanently (saved to config)

### Approval modes

Approval mode is visible in the chat status line. With no approval configuration, `chat`, `ask`, and ordinary `serve` platforms (`web`, `api`, `jobs`, and `telegram`) start in **Auto**; `edit`, `exec`, `loop`, and standalone `serve mcp` start in **Prompt**.

- **Prompt**: unapproved tool actions ask before proceeding.
- **Auto**: Guardian reviews supported actions that remain unmatched after deterministic checks. Auto is not yolo.
- **Yolo**: tool approvals auto-approve without prompting. This mode is CLI-only.

Use the canonical flag for one run:

```bash
term-llm chat --approval prompt
term-llm serve web --approval prompt
term-llm ask --approval auto "inspect this"
term-llm chat --approval yolo
```

`--auto` and `--yolo` remain aliases. Use `--approval prompt` to launch an ordinary serve platform with human approval instead of its auto default; `serve.approval_mode: prompt` makes that override persistent. Standalone `serve mcp` keeps its prompt default. In the TUI, Shift+Tab normally cycles `prompt → auto → yolo → prompt`. If no Guardian reviewer is available during setup, Shift+Tab skips auto and tells you why. After the breaker suspends auto, the first Shift+Tab explicitly resumes auto with a fresh epoch; a second press advances to yolo.

To replace the built-in matrix globally or per surface:

```yaml
approval:
  default_mode: prompt

chat:
  approval_mode: auto
```

Persistent config accepts only `prompt` and `auto`; yolo cannot be configured or restored on cold resume.

Auto mode sends these unmatched local actions to Guardian:

- shell commands;
- file reads;
- file writes, edits, and unified diffs;
- grep and glob directory searches; and
- image input and output path access; and
- explicit `manage_workspace` requests for additional session workspaces or write elevation.

Deterministic exact approvals and narrow allow patterns run first. Guardian shell approvals are cached only for the exact command and working directory; Guardian never grants a wildcard. Ordinary unmatched per-file reviews do not create directory/repository authority; only an explicit `manage_workspace` call can install a session-scoped workspace capability. Existing exact script commands, exact session commands, and exact Guardian command/workdir entries remain deterministic.

Broad shell patterns that mechanically grant arbitrary execution are suspended while auto is requested. This includes `*`, executable globs such as `*/bin/*`, wildcard interpreter/shell/elevation rules such as `python *.py`, `node *.js`, `bash *`, `env *`, and `sudo *`, and wildcard dispatcher rules such as `uv run *`, `npx *`, and `pipx run *`. These entries are not deleted and work again after an explicit switch to prompt mode; this matters for older generated approvals such as `python *`. Fixed commands such as `python script.py`, `uv run pytest`, `npx eslint`, and `pipx run black` remain narrow. A suspended pattern is not a denial—the command proceeds to Guardian while auto is active, and an approval can populate the exact command/workdir cache.

Set `guardian.classify_all_shell: true` to suspend **all** shell patterns in auto, including narrow entries such as `git status` and `go test *`. Exact approvals are still honored. Configured, session, ancestor, and project pattern lists are filtered and matched independently; term-llm never combines patterns from different sources to approve a compound command. Safe pipe targets such as `head` or `grep` can satisfy only a later pipe segment after the head has a usable deterministic approval, so they cannot resurrect a suspended head command. An explicit switch to prompt or yolo restores the previous allow-rule behavior.

Guardian denials, contradictory allows, unavailability, timeouts, malformed responses, and other review failures fail closed in terminal, web/serve, and headless runtimes. The reviewed action returns an error to the agent and does not immediately open a human approval prompt or offer a persistent command/directory grant. The agent is told not to retry the same outcome through a workaround. To override deliberately, authorize that exact action in a subsequent message so Guardian can reassess the new trusted transcript evidence, or switch the session to prompt/yolo with existing controls.

Three consecutive policy denials or 20 total policy denials in one auto epoch suspend auto for the whole parent/child manager tree. A successful Guardian approval resets the consecutive count but not the total; reviewer transport/parse failures do not count. The threshold-triggering action remains denied, and only the next approval-bearing action uses effective prompt mode. While the suspension latch is set, shell-pattern filtering still follows the requested auto policy: arbitrary-execution patterns (or every pattern with `classify_all_shell`) cannot silently begin matching just because the effective mode is prompt. In the TUI, the first Shift+Tab after suspension explicitly resumes auto with a fresh epoch instead of jumping to yolo. In web/serve, suspended approval prompts offer a separate, unchecked **Resume Guardian auto after this decision** control; approving or denying an action without that explicit response leaves auto suspended. Suspension does not rewrite the requested/persisted policy, and a cold resume may also enter auto with a fresh epoch.

Guardian reviews with an LLM by default. Setting `guardian.backend: classify` routes reviews to a classification provider (TypeSafe, the OpenAI Decisions API, or Cloudflare Clef), which returns typed risk/authorization/outcome answers instead of generated prose. Everything on this page — deterministic checks first, pattern suspension, fail-closed denials and the denial breaker — is identical for both backends; only the reviewer changes. The `guardian.provider` and `guardian.model` overrides below apply to the LLM backend only. See [Classification](/guides/classify/#guardian-backend).

Optional Guardian overrides:

```yaml
guardian:
  provider: anthropic
  model: claude-sonnet-4-6
  policy_path: ~/.config/term-llm/guardian-policy.md
  timeout_seconds: 90 # optional; default 90 seconds
  classify_all_shell: false
```

Without explicit Guardian overrides, Guardian uses the configured fast model for the global `default_provider`, switching to its `fast_provider` when configured. Per-command, per-surface, session, and agent provider/model overrides do not change that target. `guardian.provider` explicitly selects another provider and its own `fast_model` without following that provider's `fast_provider` (a `fast_model` paired with a different `fast_provider` is ignored); `guardian.model` is authoritative and stays paired with `guardian.provider`, or with the global default provider when no Guardian provider is set. For compatibility, a custom or local provider with no `fast_model` uses its configured `model` before considering a built-in fast default, which only applies on the provider's stock endpoint.

Privacy note: Guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If `guardian.provider` points at a different provider than your chat session—or fast resolution selects a separate `fast_provider`—that evidence is sent to the Guardian provider too. Pin an appropriate provider/model if that routing is not acceptable.

### Host-scoped workspace policy

Agents may set `workspace: none` to keep a runtime shell directory without proposing it as a primary workspace. In that mode `manage_workspace` is not advertised and primary-workspace confirmation is skipped, including for inherited parent proposals. This setting does not revoke separately configured file or shell authority.
