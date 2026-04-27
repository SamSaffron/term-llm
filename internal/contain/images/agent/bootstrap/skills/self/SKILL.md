---
name: self
description: "Modify this agent: system prompt, agent.yaml, scripts, or memory structure. Use when asked to update behavior, personality, memory rules, or configuration."
tools:
  - name: self_update
    description: "Pull latest term-llm source, build, install binary, and restart services. Use when asked to update or upgrade term-llm."
    script: scripts/update.sh
    timeout_seconds: 120
    input:
      type: object
      properties: {}
      additionalProperties: false
---

# Self-Modification

## When to Use
- User asks to update personality or behavior
- Changing system prompt instructions
- Updating agent.yaml config (tools, model, max_turns)
- Adding or updating skills
- Restructuring memory

## Interpretation Rule: "update yourself"
- If the user says **"update yourself"**, **"upgrade yourself"**, **"pull latest"**, or similar — use the `self_update` tool.
- Do **not** treat those phrases as prompt/config self-modification unless the user explicitly mentions behavior, personality, system prompt, or agent config.
- If both interpretations seem plausible, prefer the software update.

## CRITICAL: Never directly edit agent.yaml or system.md

Use the patch scripts via `shell`. They validate, backup, diff, and apply:

```bash
bash "$AGENT_DIR/scripts/patch-system.sh" /tmp/new-system.md
bash "$AGENT_DIR/scripts/patch-agent.sh" /tmp/new-agent.yaml
```

Where `AGENT_DIR` is `/home/agent/.config/term-llm/agents/$AGENT_NAME`.

## File Locations

| File | Purpose |
|------|---------|
| `system.md` | Personality, behavior, memory rules |
| `agent.yaml` | Tools, model, shell permissions |
| `scripts/update.sh` | Pull + build latest term-llm |
| `scripts/patch-system.sh` | Safe system.md updater |
| `scripts/patch-agent.sh` | Safe agent.yaml updater |

## Adding a New Skill

```bash
mkdir -p /home/agent/.config/term-llm/skills/<name>
# Write SKILL.md with frontmatter: name, description, tools (if needed)
term-llm skills  # verify it appears
```
