---
title: "Skills"
weight: 10
description: "Use and manage portable instruction bundles that add task-specific context."
kicker: "Portable expertise"
source_readme_heading: "Skills"
featured: true
next:
  label: Built-in tools
  url: /reference/built-in-tools/
---
Skills are portable instruction bundles that provide specialized knowledge for specific tasks. A skill can add instructions to the current conversation or, when declared with `context: fork`, run through a named agent in a fresh child session.

### Using Skills

```bash
term-llm ask --skills git "how to squash commits"   # use git skill
term-llm chat --skills git,docker                   # multiple skills
term-llm edit --skills refactoring -f main.go "refactor this"
```

### Managing Skills

```bash
term-llm skills                              # List all available skills
term-llm skills --local                      # Only local (project) skills
term-llm skills --user                       # Only user-global skills
term-llm skills --source claude              # Only Claude Code ecosystem skills
term-llm skills new my-skill                 # Create new skill
term-llm skills show git                     # Show skill content
term-llm skills edit git                     # Edit skill
term-llm skills copy builtin/git my-git      # Copy skill to customize
term-llm skills browse                       # Browse available skills
term-llm skills validate my-skill            # Validate skill syntax
term-llm skills update                       # Update skills from sources
term-llm skills path                         # Print skills directory
term-llm skills add owner/repo               # Add skill source from GitHub
```

### Skill Configuration

Skills live in `~/.config/term-llm/skills/<name>/SKILL.md`. Each skill is a directory containing a `SKILL.md` file with YAML frontmatter and Markdown body:

```markdown
---
name: git
description: "Git version control expertise"
---

# Git Skill

When helping with Git:
- Prefer rebase over merge for cleaner history
- Use conventional commit messages
- Explain the implications of destructive operations
```

**Skill search order:** local (project, nearest directory first) → user (`~/.config/term-llm/skills/`) → ecosystem paths. The first skill with a given name wins.

### Invoke a skill from chat

User-invocable skills appear alongside built-in slash commands in the terminal and Web composers. Type the exact skill name followed by optional arguments:

```text
/explain internal/config
/review "the staged changes"
/commit-message src/ internal/
```

Only an exact listed name invokes a skill. Prefixes such as `/rev` and unknown slash text remain ordinary prompt input. Built-in commands win collisions; use `/skills run <name> [arguments]` in the TUI to run a colliding skill explicitly.

Useful TUI commands:

```text
/skills list                     # user-invocable skills and execution mode
/skills show review              # metadata and source
/skills run review staged        # explicit invocation, including collisions
/skills active                   # running isolated skills
/skills cancel skill-1           # cancel only this child run
```

A normal skill runs in the current conversation. Its expanded body is stored as hidden developer context while the concise slash invocation remains visible. A `context: fork` skill starts an isolated child with no parent transcript, displays independent progress, and returns a linked result block. Forked skills can run while the parent is streaming; Esc still targets the parent response, while the skill-run cancel action or `/skills cancel` targets the child.

The browser uses the same server-owned catalog and activation service. Reloading or switching sessions reconnects to active isolated runs instead of converting skill files into client-side prompt text.

### Invocation metadata

`name` and `description` are portable Agent Skills fields. The fields below are optional **client extensions** supported for compatibility with Claude-style skill clients; they are not part of the portable Agent Skills core format:

| Field | Default | Meaning |
|---|---:|---|
| `user-invocable` | `true` | Set `false` for a model-only skill that should not appear as a slash command. |
| `disable-model-invocation` | `false` | Set `true` for a manual-only skill hidden from model discovery and activation. |
| `argument-hint` | empty | Completion hint such as `[scope]` or `<issue> [notes]`. |
| `context` | `main` | `main` adds context to the active conversation; `fork` starts a fresh child. |
| `agent` | `developer` for forks | Agent used for `context: fork`. It must resolve before a child session starts. |
| `model` | agent default | Optional forked-run model override, using the same aliases or `provider:model` form as agents. |

Unknown extension fields remain available to compatible clients. Known fields are validated strictly: booleans must be booleans, strings must be strings, and `context` must be `main` or `fork`.

Arguments use deterministic shell-like tokenization: whitespace separates values, single and double quotes group values, and backslashes escape characters. There is no shell, environment-variable, tilde, command, or glob expansion. `$ARGUMENTS` expands to the original raw argument text and zero-based `$ARGUMENTS[N]` expands to one parsed value. Expansion is one pass, so placeholder-looking text supplied by a user is not expanded again. If the body contains no placeholder, non-empty arguments are appended under an `Invocation arguments` heading.

#### Main-context example

```markdown
---
name: explain
description: Explain a code path in the current conversation
argument-hint: <path-or-symbol>
disable-model-invocation: true
---

Explain `$ARGUMENTS` using the current conversation and repository context.
Do not edit files unless the user asks.
```

`disable-model-invocation: true` makes this manual-only: `/explain ...` works, but the model cannot discover or activate it.

#### Isolated review example

```markdown
---
name: review
description: Review changes without consuming the parent transcript
argument-hint: [scope]
context: fork
agent: reviewer
model: fast
allowed-tools: [read_file, grep, shell]
---

Review $ARGUMENTS for correctness, regressions, and missing tests.
Report findings with file and line references. Do not modify files.
```

The reviewer sees the expanded skill instructions and the live workspace, but no parent conversation messages. Parent and child may read or modify the same working directory concurrently: results reflect the files observed during the run, not an immutable snapshot. Existing path restrictions, approvals, and tool permissions remain in force.

#### Commit-message and model-only examples

```markdown
---
name: commit-message
description: Draft a commit message in an isolated specialist
context: fork
agent: commit-message
argument-hint: [scope]
allowed-tools: [shell]
---

Inspect the requested diff ($ARGUMENTS) and return one concise imperative commit message.
```

```markdown
---
name: internal-conventions
description: Repository conventions for the model to load when useful
user-invocable: false
---

Follow the repository's internal conventions...
```

`user-invocable: false` makes the second skill model-only. `never_auto` is different: it keeps a skill out of automatic model metadata while the skill remains visible to users.

### Tool restrictions for direct invocation

`allowed-tools` is a restrictive allowlist, never a permission grant. The skill's list is intersected with existing policy:

- omitted `allowed-tools` adds no new restriction;
- `allowed-tools: []` blocks every tool for that activation;
- a non-empty list permits only those names that existing policy already allows;
- script-backed `tools` declared by the skill are registered before filtering, but still require inclusion in a present allowlist.

For isolated runs, normal approvals still apply to mutations. Cancellation preserves any partial final output returned by the child and keeps the child transcript linked for inspection. On quit, reload, or server shutdown, active child runners are cancelled and drained before their session store is closed.

### Structured Web API

First-party browser dispatch uses session-bound endpoints rather than reading `SKILL.md` in JavaScript:

```text
GET    /v1/sessions/{session-id}/skills
POST   /v1/sessions/{session-id}/skills/invoke
GET    /v1/sessions/{session-id}/skill-runs/{run-id}
GET    /v1/sessions/{session-id}/skill-runs/{run-id}/events
DELETE /v1/sessions/{session-id}/skill-runs/{run-id}
```

The invocation body is `{"name":"review","arguments":"staged"}`. The server resolves execution mode and returns either a normal response stream ID or an isolated run ID, child session ID, and event URL. Requests pass the server's normal bearer-token authentication and must send an `X-Term-LLM-Session-ID` header matching the URL; the legacy `session_id` header remains accepted for compatibility. Run lookup also verifies that the run belongs to that session. This is a request-consistency check, not separate per-client authorization: a holder of the server token can access other server sessions by their IDs.

### Configuration

Skills are enabled by default. To disable them globally, set `enabled: false` in `~/.config/term-llm/config.yaml`:

```yaml
skills:
  enabled: false
```

The `--skills` flag on any command implicitly enables the system for that invocation, so `term-llm ask --skills git "..."` works even when skills are disabled in config. Agents can also enable skills via their `skills` field. All built-in agents set `skills: "all"`, so skills are active whenever you use a built-in agent.

Full configuration reference:

```yaml
skills:
  enabled: true                   # Master switch (default: on)
  auto_invoke: true               # Let the model call activate_skill on its own
  metadata_budget_tokens: 8000    # Max tokens for skill metadata in system prompt
  max_visible_skills: 50           # Max skills shown in system prompt (0 = search only)
  include_project_skills: true    # Discover from .skills/ in project tree
  include_ecosystem_paths: true   # Discover from ~/.claude/skills/, ~/.codex/skills/, etc.
  always_enabled: []              # Always include these skills in metadata
  never_auto: []                  # Require explicit --skills or activate_skill call
```

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Master switch. Must be `true` for auto-invocation to work. |
| `auto_invoke` | `true` | When enabled, the model can call `activate_skill` without being asked. |
| `metadata_budget_tokens` | `8000` | Token budget for the `<available_skills>` block injected into the system prompt. |
| `max_visible_skills` | `50` | Maximum number of skills shown in the system prompt. When more skills exist, `search_skills` is registered automatically. Set to `0` for pure search mode. |
| `include_project_skills` | `true` | Scan `.skills/` directories from CWD up to the repo root. |
| `include_ecosystem_paths` | `true` | Also scan `.claude/skills/`, `.codex/skills/`, `.gemini/skills/`, `.cursor/skills/` at both project and user scope. |
| `always_enabled` | `[]` | Skills that are always included in metadata, even if they would exceed the budget. |
| `never_auto` | `[]` | Skills excluded from auto-discovery metadata. They can still be activated with `--skills name` or explicit `activate_skill` calls. |

### The `--skills` flag

The `--skills` flag overrides config for a single invocation:

| Value | Effect |
|-------|--------|
| `--skills git` | Enable skills system, load `git` as always-enabled, disable auto-invoke for others |
| `--skills git,docker` | Same, with multiple skills |
| `--skills git,+` | Load `git` explicitly **and** keep auto-invoke on for remaining skills |
| `--skills all` | Enable all skills with auto-invoke |
| `--skills none` | Disable skills entirely for this command |

### How auto-loading works

When `skills.enabled` is `true`, this happens at startup:

1. The registry scans all search paths (project-local, user, ecosystem) and discovers available skills
2. Skills in `never_auto` are excluded from the metadata list
3. The remaining skills are truncated to fit `max_visible_skills` and `metadata_budget_tokens` (skills in `always_enabled` are never truncated)
4. An `<available_skills>` XML block is injected into the system prompt with skill names and descriptions
5. The `activate_skill` tool is registered. When the model calls it, the full skill body is loaded and returned
6. If there are more skills than shown, the `search_skills` tool is also registered and a hint is added to the system prompt telling the model to use it

When `auto_invoke` is `false`, model-facing skill metadata and model activation are disabled, but user-invocable skills remain available through slash completion and `/skills run`.

### Skill Tools

Skills can declare script-backed tools in their frontmatter. When the skill is activated via `activate_skill`, those tools are dynamically registered with the engine. The LLM can then call them directly, with no hardcoded paths anywhere.

```markdown
---
name: google-maps
description: "Google Maps queries: travel times, place search, geocoding"
tools:
  - name: maps_travel_time
    description: "Get traffic-aware travel time between two locations"
    script: scripts/travel-time.sh
    timeout_seconds: 15
    input:
      type: object
      properties:
        origin:
          type: string
          description: "Origin address or lat,lng"
        destination:
          type: string
          description: "Destination address or lat,lng"
        mode:
          type: string
          description: "DRIVE, WALK, BICYCLE, or TRANSIT (default: DRIVE)"
      required: [origin, destination]

  - name: maps_places_search
    description: "Free-text place search with optional location bias"
    script: scripts/places-search.sh
    input:
      type: object
      properties:
        query:
          type: string
        latlng:
          type: string
          description: "Optional bias point as lat,lng"
      required: [query]
---

# Google Maps Skill

API key is embedded in the scripts. No need to handle it here.
...
```

Scripts live in the skill directory (e.g. `scripts/travel-time.sh`) and receive the LLM's arguments as **JSON on stdin**, exactly like agent custom tools:

```bash
#!/usr/bin/env bash
INPUT=$(cat)
ORIGIN=$(echo "$INPUT" | jq -r '.origin')
DESTINATION=$(echo "$INPUT" | jq -r '.destination')
MODE=$(echo "$INPUT" | jq -r '.mode // "DRIVE"')
# ... call the API
```

This is the recommended pattern for skills that need API keys or other secrets. The key lives only in the script, never in the SKILL.md body that gets injected into the LLM context.

**Field reference** (same as agent custom tools):

| Field | Required | Description |
|-------|----------|-------------|
| `name` | ✓ | Tool name shown to LLM. Must match `^[a-z][a-z0-9_]*$` |
| `description` | ✓ | Description passed to LLM in the tool spec |
| `script` | ✓ | Path relative to the skill directory (e.g. `scripts/foo.sh`) |
| `input` | | JSON Schema for parameters. Must be `type: object` at root |
| `timeout_seconds` | | Execution timeout (default 30, max 300) |
| `env` | | Extra environment variables when running the script |

Scripts run with `TERM_LLM_AGENT_DIR` set to the skill's directory and `TERM_LLM_TOOL_NAME` set to the tool name. Symlinks are resolved and containment-checked. Scripts cannot escape the skill directory.
