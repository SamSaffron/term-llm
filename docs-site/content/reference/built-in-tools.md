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
| `shell` | Execute shell commands; accepts optional `affected_paths` hints so file-change tracking can snapshot generated/modified files reliably |
| `grep` | Search file contents (uses ripgrep) |
| `glob` | Find files by glob pattern |
| `view_image` | Inspect an image file. Normally returns structured image content to a vision-capable primary model; with `vision_via`, calls the configured vision model and returns text only. |
| `show_image` | Show image file info |
| `image_generate` | Generate images via configured provider |
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

### File-change tracking hints

When [file change tracking](/reference/sessions/#file-change-history/) is enabled, direct write tools (`write_file`, `edit_file`, `unified_diff`) are recorded automatically. The `shell` tool can also record files it creates, modifies, or deletes. For shell commands that generate files, pass `affected_paths` so term-llm can snapshot exactly what matters before and after the command:

```json
{
  "command": "npm run build",
  "working_dir": "/path/to/project",
  "affected_paths": ["dist/**", "package-lock.json"]
}
```

Hints may be files or glob patterns, relative to `working_dir` or absolute. Without hints, shell tracking falls back to `git status` in repositories and files already touched by the session, which is useful but intentionally best-effort.

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

When a tool needs access outside approved directories, term-llm prompts for approval with options:
- **Proceed once**: Allow this specific action
- **Proceed always**: Allow for this session (memory only)
- **Proceed always + save**: Allow permanently (saved to config)

### Approval modes

Interactive chat has three approval modes, shown in the status line when active:

- **Prompt** (default): unapproved tool actions ask before proceeding.
- **Auto**: unmatched shell commands are reviewed by a guardian model before falling back to the human prompt. Path/file/MCP approvals still prompt.
- **Yolo**: tool approvals auto-approve without prompting.

Use flags for one run:

```bash
term-llm chat --auto
term-llm chat --yolo
```

In the TUI, Shift+Tab cycles `prompt → auto → yolo → prompt`. If no guardian reviewer is configured, Shift+Tab skips auto and tells you why.

Auto mode is intentionally narrow. It does not turn shell approval into a glob pattern and does not grant broad shell access. The guardian reviews the exact command and working directory, transcript/tool evidence, and deterministic approval context such as configured/session read and write directories. That lets it recognize narrow shell equivalents of already-approved first-party file operations while still denying or escalating unrelated shell side effects, network transfer, process control, or credential disclosure.

Guardian outcomes are added to scrollback. In terminal chat, if guardian denies or cannot review and a human approval prompt appears, the guardian rationale is repeated immediately above the prompt so you can scroll up and read why you are approving. In web/serve mode, guardian review messages are emitted into the response stream before any approval prompt.

To make new chat sessions start in auto mode:

```yaml
approval:
  default_mode: auto
```

Optional guardian overrides:

```yaml
guardian:
  provider: anthropic
  model: claude-sonnet-4-6
  policy_path: ~/.config/term-llm/guardian-policy.md
  timeout_seconds: 90 # optional; default 90 seconds
```

Privacy note: guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If `guardian.provider` points at a different provider than your chat session, that evidence is sent to the guardian provider too. Leave `guardian.provider` unset to avoid routing approval evidence to an additional provider.
