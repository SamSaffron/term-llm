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

| Tool | Description |
|------|-------------|
| `read_file` | Read file contents (with line ranges) |
| `write_file` | Create/overwrite files |
| `edit_file` | Edit existing files |
| `shell` | Execute shell commands; when file tracking is enabled, `affected_paths` bounds inspection and `output_claims` declares transform, generated deliverable, or materialization intent |
| `grep` | Search file contents (uses ripgrep) |
| `glob` | Find files by glob pattern |
| `view_image` | Inspect an image file. Normally returns structured image content to a vision-capable primary model; with `vision_via`, calls the configured vision model and returns text only. |
| `show_image` | Show image file info |
| `image_generate` | Generate images via configured provider |
| `manage_workspace` | Grant, list, or revoke session-scoped local workspaces. Added automatically whenever a local path-capable file/search/image tool is enabled. |
| `ask_user` | Prompt user for input |
| `create_goal` / `get_goal` / `update_goal` | Create/read or complete/block a persistent `/goal` (goal tools are injected automatically while a goal is active) |
| `spawn_agent` | Spawn child agents for parallel tasks |
| `run_agent_script` | Run a script bundled in the agent directory |
| `activate_skill` | Activate a skill by name |

### Indirect image understanding for text-only models

For a text-only model that supports tool calls, add `vision_via` to that model's provider entry:

```yaml
providers:
  local-text:
    type: openai_compatible
    model: qwen-text
    vision_via: gemini
```

Set `vision_via` either at provider level, as above, or on a specific `models:` object when only one model should use the route or needs a different vision backend. Use `provider` to select that provider's default model, or `provider:model` to force a specific model. It inserts a prompt reference such as `[User uploaded image: /.../uploads/image_123.png — use view_image ...]`, auto-enables `view_image`, and lets the model call `view_image` with `file_path` plus an optional `question`. The tool then forwards the processed image to the configured vision-capable provider/model and returns a textual analysis.

Limitations: the primary model must call tools; the `vision_via` provider must be configured and able to process image parts; and `view_image` can only read uploaded images or paths allowed through normal read permissions/approvals.

### Filesystem change attribution

[File change tracking](/reference/sessions/#file-change-history/) is enabled by default. When enabled, trusted direct write tools (`write_file`, `edit_file`, `unified_diff`) record witnessed transitions automatically. Shell detection is different: `affected_paths` only bounds pre/post inspection and never creates attribution. Declare intended task output before execution with `output_claims`:

```json
{
  "command": "gofmt -w ./internal/... && go generate ./cmd/...",
  "working_dir": "/path/to/project",
  "affected_paths": ["internal/**", "cmd/**"],
  "output_claims": [
    {"path": "internal/**/*.go", "kind": "transform"},
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

    - name: job_run
      description: "Trigger a scheduled job to run immediately."
      script: scripts/job-run.sh
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
      script: scripts/job-history.sh
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

Scripts receive the LLM's arguments as **JSON on stdin**:

```bash
#!/usr/bin/env bash
INPUT=$(cat)
NAME=$(echo "$INPUT" | jq -r '.name')
LIMIT=$(echo "$INPUT" | jq -r '.limit // 10')
sqlite3 "$DB_PATH" \
  "SELECT * FROM runs WHERE job='$NAME' ORDER BY started DESC LIMIT $LIMIT;"
```

**Field reference:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | ✓ | Tool name shown to LLM. Must match `^[a-z][a-z0-9_]*$`, no collisions with built-in names |
| `description` | ✓ | Description passed to LLM in the tool spec |
| `script` | ✓ | Path to script, relative to the agent directory (e.g. `scripts/foo.sh`) |
| `input` | | JSON Schema for parameters. Must be `type: object` at root. If omitted, tool takes no parameters |
| `timeout_seconds` | | Execution timeout (default 30, max 300) |
| `env` | | Extra environment variables to set when running the script |

Scripts run with `TERM_LLM_AGENT_DIR` and `TERM_LLM_TOOL_NAME` set. Symlinks are resolved and containment-checked. Scripts cannot escape the agent directory. No approval prompt is shown; scripts in the agent directory are implicitly trusted.

### Tool Permissions

Control which directories and commands tools can access:

```bash
# Allow read access to specific directories
term-llm chat --tools read,grep --read-dir /home/user/projects

# Allow write access to specific directories
term-llm chat --tools read,write,edit --read-dir . --write-dir ./src

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

Optional Guardian overrides:

```yaml
guardian:
  provider: anthropic
  model: claude-sonnet-4-6
  policy_path: ~/.config/term-llm/guardian-policy.md
  timeout_seconds: 90 # optional; default 90 seconds
  classify_all_shell: false
```

Without explicit Guardian overrides, Guardian uses the configured fast model for the global `default_provider`, switching to its `fast_provider` when configured. Per-command, per-surface, session, and agent provider/model overrides do not change that target. `guardian.provider` explicitly selects another provider and its fast model without following that provider's `fast_provider`; `guardian.model` is authoritative and stays paired with `guardian.provider`, or with the global default provider when no Guardian provider is set. For compatibility, a custom or local provider with no `fast_model` uses its configured `model` before considering a built-in fast default.

Privacy note: Guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If `guardian.provider` points at a different provider than your chat session—or fast resolution selects a separate `fast_provider`—that evidence is sent to the Guardian provider too. Pin an appropriate provider/model if that routing is not acceptable.
