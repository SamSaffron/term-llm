---
name: memory
description: "Read and update persistent memory. Use when: user says 'remember', asks about past context, preferences, or projects. Also use proactively to save anything reusable before a conversation ends."
---

# Memory System

Memory is stored as **fragments** in a SQLite database, indexed with BM25 + vector embeddings.
Mining runs frequently and `recent.md` is refreshed from fragments. Garbage collection is manual only; decay is a review signal, not a deletion policy.

## Lookup — always search first

```bash
term-llm memory search "<query>" --agent "$AGENT_NAME" --limit 6
```

Hybrid BM25 + vector search — use it for anything that might be in memory. Search is relevance-only by default; use `--freshness`/`--recency` only when newer matching facts should win. Use `--touch` only when you explicitly want to record access metadata.

Show full fragment by numeric ID (preferred — no truncation):

```bash
term-llm memory fragments show 42 --agent "$AGENT_NAME"
```

List and filter:

```bash
term-llm memory fragments list --agent "$AGENT_NAME" --limit 10
term-llm memory fragments list --agent "$AGENT_NAME" --filter-path telegram
```

## Writing new memory

Use the CLI to add or update fragments directly in the database:

```bash
term-llm memory fragments add fragments/<category>/<name>.md --agent "$AGENT_NAME" --content "..."
term-llm memory fragments update fragments/<category>/<name>.md --agent "$AGENT_NAME" --content "..."
```

Delete only when the user explicitly asks or when replacing a provably wrong fragment:

```bash
term-llm memory fragments delete fragments/<category>/<name>.md --agent "$AGENT_NAME"
```

Category guide:
- `preferences/` — stable user and agent behavior preferences
- `people/` — user/family/profile facts
- `homelab/` — infrastructure, services, network, runbooks
- `projects/` — long-lived project context, decisions, scars
- `skills/` — skill-specific operating notes
- `tools/` — external tools, APIs, tool behavior
- `snippets/` — reusable commands/workflows
- `notes/` — unclassified knowledge; promote periodically into better rooms
- `bugs/` — open bugs or memorable scars
- `feedback/` — user corrections; fold into preferences/personality over time
- `credentials/` — credential locations and handling rules only; never raw secrets
- `index/` — maps and palace navigation

## Memory palace

Use `memory/palace.md` as the compact map of durable context that should be visible every session. Keep it tiny and room-like:

```md
## Shape rules
- Prefer `fragments/<room>/<shelf>/<topic>.md`.
- Avoid per-session/per-PR/per-update fragments unless they are active work.
- Store credential locations, not raw secrets.

## User / preferences
- Stable preferences, collaboration style, durable constraints.

## People
- User/family/profile facts.

## Projects
- Long-lived project facts and decisions.

## Homelab / infrastructure
- Repos, services, commands, credential locations (never secrets).

## Tools / skills
- Tool behavior and skill operating notes.

## Open loops
- Durable unresolved work only; remove resolved loops during consolidation.
```

Do not dump transcripts into the palace. Promote durable facts into fragments first, then consolidate the current-state summary.

## Consolidation

Preview consolidation first:

```bash
term-llm memory consolidate --agent "$AGENT_NAME"
```

Apply only when the preview is good:

```bash
term-llm memory consolidate --agent "$AGENT_NAME" --apply
```

`memory consolidate` is **current-state consolidation**: it rewrites `recent.md` from changed fragments and prints a non-destructive decay preview. It does not perform full palace cleanup, delete fragments, move fragments, or rewrite stored decay scores.

For full palace cleanup, work room-by-room: back up source rows, create/update a canonical fragment, then explicitly delete superseded source fragments only after verifying search still finds the canonical memory.

Backup convention before deleting many fragments:

```bash
mkdir -p /home/agent/artifacts/memory-consolidation/$(date +%F)
# Write source rows for the cluster there before deleting anything.
```

Recommended cadence:
- Mine sessions frequently (for example every 30 min).
- Run `memory consolidate --apply` daily to keep `recent.md` tidy.
- Run deep palace cleanup manually or as a weekly dry-run/report; apply only with explicit judgment.

## When user says "remember this"

1. Add the fragment immediately via CLI
2. If the fact is stable and globally useful, keep the palace/recent summary in mind for the next consolidation
3. Confirm to user

## Memory rules

- **Search before answering** anything about user preferences, history, or projects
- `recent.md` is loaded at session start and maintained from fragments — do not hand-edit it casually
- Prefer updating source fragments, then consolidating
- The session miner handles most things automatically after conversations end
- Proactively create fragments when content is structured or unlikely to survive miner summarisation

## Job schedule (created by bootstrap-jobs)

| Job | Schedule | Purpose |
|-----|----------|---------|
| `mine-sessions` | every 30 min | Extract fragments from transcripts |
| `update-recent` | every 10 min | Promote fragments into recent.md |
