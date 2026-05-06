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
Skills are portable instruction bundles that provide specialized knowledge for specific tasks. Unlike agents, skills don't change the provider or model. They just add context.

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

**Skill search order:** local (project) → user (`~/.config/term-llm/skills/`) → built-in

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

When `auto_invoke` is `false`, the `<available_skills>` block is still injected but the model won't call `activate_skill` on its own. It only fires when you pass `--skills name`.

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
