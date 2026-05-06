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
| `shell` | Execute shell commands |
| `grep` | Search file contents (uses ripgrep) |
| `glob` | Find files by glob pattern |
| `view_image` | Display images in terminal (icat) |
| `show_image` | Show image file info |
| `image_generate` | Generate images via configured provider |
| `ask_user` | Prompt user for input |
| `spawn_agent` | Spawn child agents for parallel tasks |
| `run_agent_script` | Run a script bundled in the agent directory |
| `activate_skill` | Activate a skill by name |

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
