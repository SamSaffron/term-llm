---
title: "Agents"
weight: 9
description: "Use built-in agents or create your own workflow bundles with their own provider, model, tools, and instructions."
kicker: "Workflow bundles"
source_readme_heading: "Agents"
featured: true
next:
  label: Skills
  url: /guides/skills/
---
Agents are named workflow bundles. An agent can carry its own system prompt, provider and model preferences, tool permissions, MCP servers, shell allowlists, and turn limits.

That means you can stop restating the same intent over and over. Use `@reviewer` when you want review behavior, `@codebase` when you want repository exploration, `@web-researcher` when you want web-backed investigation, and so on.

## Using agents

Use the `@agent` prefix syntax or `--agent` flag:

```bash
term-llm ask @reviewer "review this code" -f main.go
term-llm chat @codebase
term-llm ask --agent web-researcher "Find info about Go 1.24"
```

## Built-in agents

List them anytime with:

```bash
term-llm agents list --builtin
```

term-llm ships with these built-in agents:

| Agent | What it is for |
|---|---|
| `active-review` | Runs a review-and-fix loop by spawning `reviewer`, then `developer` if fixes are needed. |
| `agent-builder` | Creates and edits custom agents interactively. |
| `artist` | Image generation and editing workflows. |
| `changelog` | Writes human-readable summaries of interesting git activity. |
| `codebase` | Reads repositories, traces call paths, and answers source-code questions. |
| `commit-message` | Writes commit messages from staged or unstaged changes. |
| `contain` | Authors and runs Docker Compose workspaces under `~/.config/term-llm/containers`. |
| `developer` | Implements code changes, fixes, and features. |
| `editor` | Focused file editing without shell access. |
| `file-organizer` | Renames and organizes files into sensible names and folders. |
| `web-researcher` | Information gathering with web search. |
| `reviewer` | Read-only code review with git-aware inspection tools. |
| `shell` | General shell command helper. |

A few good starting points:

- `@reviewer` for code review without letting the model edit files
- `@codebase` for architecture questions and tracing behavior across a repo
- `@developer` when you want implementation work done
- `@web-researcher` when the answer depends on current web information
- `@commit-message` when you want a clean commit message without fuss

## Managing agents

```bash
term-llm agents                              # List all agents
term-llm agents list                         # Same as above
term-llm agents list --builtin               # Only built-in agents
term-llm agents list --local                 # Only local agents
term-llm agents list --user                  # Only user agents
term-llm agents new my-agent                 # Create new agent
term-llm agents show reviewer                # Show agent configuration
term-llm agents edit reviewer                # Edit agent configuration
term-llm agents copy reviewer my-reviewer    # Copy an agent to customize
term-llm agents path                         # Print agents directory
term-llm agents export reviewer              # Export an agent bundle
term-llm agents import ./agent-dir           # Import an agent bundle
term-llm agents gist reviewer                # Publish agent as a gist
term-llm agents set reviewer provider=openai model=gpt-5.2
term-llm agents get reviewer
term-llm agents clear reviewer model
```

## Customizing built-ins

A filesystem agent with the same name as a built-in shadows the built-in. For example, to customize the default `developer` behavior, create `term-llm-agents/developer/agent.yaml` in a project or `~/.config/term-llm/agents/developer/agent.yaml` for your user account. You can start from the bundled implementation with:

```bash
term-llm agents copy developer developer      # copy built-in developer into your user agents dir
term-llm agents copy developer my-developer   # fork it under a new name instead
```

Per-agent preferences in your main config can override provider/model choices without copying an agent bundle:

```bash
term-llm agents set reviewer provider=openai model=gpt-5.2
```

The special agent model value `fast` resolves through the active provider's fast-model settings: `providers.<active>.fast_model`, or `providers.<active>.fast_provider` plus `fast_model` when the lightweight model should run on a different provider key. It is not sent to the provider as a literal model named `fast`.

Built-in `developer` can spawn up to three subagents in parallel. Its built-in spawn configuration runs spawned `codebase` subagents with `model: fast`, so developer-led repository investigations use the lightweight model without changing direct `@codebase` sessions:

```yaml
spawn:
  agent_models:
    codebase: fast
```

## Agent configuration

Agents are YAML files stored in `~/.config/term-llm/agents/`:

```yaml
# ~/.config/term-llm/agents/reviewer/agent.yaml
name: reviewer
description: Reviews code for best practices and potential issues

provider: anthropic
model: claude-sonnet-4-6

tools:
  enabled: [read_file, grep, glob]
  # OR use a denylist instead:
  # disabled: [shell, write_file]

shell:
  allow: ["git *", "npm test"]  # glob patterns for allowed commands
  auto_run: true                   # skip confirmation for matched commands
  scripts:                         # named shortcuts (auto-approved)
    build: "npm run build"

search: true   # enables web_search and read_url tools
max_turns: 50   # agentic loop limit; CLI --max-turns overrides this

mcp:
  - name: github
```

Built-in agents that currently default to `search: true`: `agent-builder`, `web-researcher`, `developer`, `editor`, `shell`, `contain`.

## Structured ask output

Use `output_tool` when an agent must return a machine-consumable artifact to `term-llm ask` instead of relying on freeform prose. In `ask`, a configured output tool is the agent's final return channel: successful completion requires the model to call that tool. If the model naturally finishes without calling it, `ask` runs a constrained finalization pass that preserves the original tool context, forces the output tool when the provider supports named tool choice, and otherwise strongly instructs the model that the task is done and it must call the output tool. If the tool still is not called, `ask` fails nonzero and does not run `on_complete`.

Define a typed object `schema` when the final result has multiple parameters:

```yaml
name: scanner

tools:
  enabled: [read_file, grep, glob]

output_tool:
  name: submit_result
  description: "Submit the final scanner result"
  schema:
    type: object
    properties:
      summary:
        type: string
      severity:
        type: string
        enum: [low, medium, high]
      files:
        type: array
        items:
          type: string
    required: [summary, severity, files]
    additionalProperties: false

on_complete: |
  jq -e . > result.json
```

For a typed schema, `on_complete` receives the complete argument object as compact JSON. The schema root must have `type: object`. Providers enforce the field constraints; validate the JSON again in `on_complete` when correctness is critical.

The legacy `param` shorthand remains available when the tool should capture one string value:

```yaml
output_tool:
  name: set_commit_message
  param: message
  description: "Set the commit message"
```

`param` defaults to `content` when omitted, and `param` and `schema` cannot be configured together. For this shorthand, `on_complete` receives only the captured string rather than a JSON object. When `output_tool` is configured, `ask` never falls back to passing assistant prose to `on_complete`.

`output_tool` is ignored in `term-llm chat`. Chat is an open-ended conversation with no single final return value; use `ask` for tool-captured output.

## Platform developer messages

When term-llm serves an agent on different platforms (web UI, Telegram, CLI chat, background jobs), each platform may need different behavioral guidance. The `platform_messages` block in `agent.yaml` lets you inject a developer-role message at the start of every new session, keyed by platform.

```yaml
platform_messages:
  web_developer_message: |
    You are running in the web UI. Use markdown, tables, and links freely.
  telegram_developer_message: |
    You are running as a Telegram bot. Keep responses short.
  chat_developer_message: |
    You are running in CLI chat mode.
  jobs_developer_message: |
    You are running as a background job. Do not prompt for input.
```

Supported platform keys:

| Key | Platform | When used |
|---|---|---|
| `web_developer_message` | Web UI / HTTP API | `term-llm serve --platform web` |
| `telegram_developer_message` | Telegram bot | `term-llm serve --platform telegram` |
| `chat_developer_message` | CLI chat | `term-llm chat` |
| `jobs_developer_message` | Scheduled/background jobs | `term-llm serve --platform jobs` |

Messages are injected as `developer` role messages before the first user turn. If no message is configured for the active platform, nothing is injected. Each key is optional. Define only the platforms you need.

## System prompt file includes

System prompts support inline file includes with `{{file:...}}`.

```md
You are a reviewer.

{{file:prompts/rules.md}}
{{file:/absolute/path/to/shared-context.md}}
```

Behavior:
- Includes are expanded recursively (max depth: 10)
- Cycles are detected and reported as errors
- Missing/unreadable include files fail fast
- Included content is inserted raw (no automatic headers/separators)
- Relative paths are source-relative:
  - Agent prompts resolve relative to the agent directory
  - Config/CLI system prompts resolve relative to the current working directory
- Expansion order is include first, then template variables (for example `{{year}}`)

System prompt templates can also read environment variables from the `term-llm` process:

```md
Running in container: {{env:DV_CONTAINER_NAME}}
```

Whitespace around the directive is allowed, for example `{{ env : DV_CONTAINER_NAME }}`. Unset environment variables expand to an empty string. Use `{{!env:DV_CONTAINER_NAME}}` to render the placeholder literally. Because values are inserted into the model-visible system prompt, avoid referencing secret env vars unless you intentionally want to send them to the model.

**Agent search order:** project-local agents (`./term-llm-agents`) → user agents (`~/.config/term-llm/agents`) → configured `agents.search_paths` → built-in agents
