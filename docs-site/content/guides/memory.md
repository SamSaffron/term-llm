---
title: "Memory"
weight: 9
featured: true
description: "Mine durable facts from completed sessions, search fragments, manage insights, and understand how memory differs from sessions."
kicker: "Long-term memory"
next:
  label: Agents
  url: /guides/agents/
---
## What memory is for

term-llm has a long-term memory system separate from normal chat sessions.

- **Sessions** store conversation history so you can resume, inspect, export, or search a chat.
- **Memory** stores durable fragments and behavioral insights mined from completed sessions.

The distinction matters. Most session content is ephemeral. Memory is meant for facts, decisions, preferences, and patterns worth carrying forward.

## The main commands

```bash
term-llm memory status
term-llm memory search "retry policy"
term-llm memory consolidate --agent assistant
term-llm memory fragments list
term-llm memory insights list --agent assistant
```

Use `--agent` when you want to scope memory to a particular agent:

```bash
term-llm memory status --agent assistant
term-llm memory search "docs tone" --agent assistant
```

## Mine completed sessions into fragments

```bash
term-llm memory mine
```

This scans completed sessions, extracts durable memory, and stores it as fragments in the memory database.

Useful flags:

```bash
term-llm memory mine --since 24h
term-llm memory mine --limit 20
term-llm memory mine --include-subagents
term-llm memory mine --dry-run
term-llm memory mine --embed=false
term-llm memory mine --insights
```

What the mining pass is trying to keep:

- stable preferences
- technical details that are painful to rediscover
- decisions and conventions
- important project and infrastructure facts

What it should mostly ignore:

- transient debugging noise
- filler conversation
- one-off dead ends
- resolved junk that does not matter later

## Search memory

```bash
term-llm memory search "oauth token handling"
```

Search uses hybrid retrieval when embeddings are available:

- BM25 keyword search
- vector similarity search
- MMR reranking

Search is read-only by default. It does not apply decay/freshness scoring unless you opt in, and it does not bump access metadata unless you pass `--touch`.

Useful flags:

```bash
term-llm memory search "provider routing" --limit 10
term-llm memory search "deploy key" --bm25-only
term-llm memory search "session persistence" --freshness
term-llm memory search "recent deploy issue" --recency --touch
term-llm memory search "embedding provider" --embed-provider gemini
```

### Freshness and decay

Freshness is now an explicit retrieval choice, not a default. `--freshness`/`--recency` apply a non-persistent timestamp multiplier at query time. `--decay` applies the stored decay score for compatibility with older databases. Neither option rewrites memory.

Garbage collection is manual and destructive. If you use it, preview first:

```bash
term-llm memory fragments gc --agent assistant
term-llm memory fragments gc --agent assistant --preview-decay
# Destructive cleanup is explicit:
term-llm memory fragments gc --agent assistant --delete
```

## Inspect fragments directly

List fragments:

```bash
term-llm memory fragments list --agent assistant --limit 20
term-llm memory fragments list --filter-path preferences
```

Show a fragment by numeric ID or path:

```bash
term-llm memory fragments show 42 --agent assistant
term-llm memory fragments show fragments/preferences/editor.md --agent assistant
```

Show where a fragment came from:

```bash
term-llm memory fragments sources 42 --json
```

Manage fragments manually when you need precise control:

```bash
term-llm memory fragments add fragments/preferences/search.md --agent assistant --content "Prefer Exa with Brave fallback."
term-llm memory fragments update fragments/preferences/search.md --agent assistant --content "Prefer Exa with Brave as fallback only."
term-llm memory fragments delete fragments/preferences/search.md --agent assistant
```

## Promote recent fragments into an agent summary

```bash
term-llm memory promote --agent assistant
```

Promotion condenses recently changed fragments into an agent-level `recent.md` summary. It is a compression step, not a raw dump.

Useful flags:

```bash
term-llm memory promote --agent assistant --since 6h
term-llm memory promote --agent assistant --recent-max-bytes 20000
term-llm memory promote --agent assistant --dry-run
```

## Consolidate safely

```bash
term-llm memory consolidate --agent assistant
term-llm memory consolidate --agent assistant --apply
```

`consolidate` is the safer current-state workflow. It defaults to a dry run, prints the proposed `recent.md` rewrite, and shows a non-destructive decay preview. `--apply` writes `recent.md`; it does not delete fragments, move fragments, perform full palace cleanup, or rewrite stored decay scores.

Use it when you want to compact recent working memory without treating age as permission to forget.

### Cadence

A practical schedule is:

- run `memory mine` frequently, such as every 30 minutes, to extract durable candidates from completed sessions
- run `memory consolidate --apply` daily to keep `recent.md` tidy
- run deep palace cleanup manually, or as a weekly dry-run/report, because it involves moving/deleting source fragments

Deep palace cleanup means clustering old fragments, backing up source rows, writing a canonical replacement fragment, and explicitly deleting superseded sources after review. It is intentionally not automatic.

## Memory palace template

Fresh agent containers seed `memory/palace.md` alongside `memory/recent.md`. The palace is a tiny, durable map of high-value context that should be visible every session. Keep detailed facts in fragments. Keep `recent.md` for current-state working memory. Keep the palace short enough that every line earns prompt budget.

Recommended room taxonomy:

```text
fragments/index/        maps and palace navigation
fragments/preferences/  stable user and agent behavior preferences
fragments/people/       user/family/profile facts
fragments/homelab/      infrastructure, services, network, runbooks
fragments/projects/     long-lived project context, decisions, scars
fragments/skills/       skill-specific operating notes
fragments/tools/        external tools, APIs, tool behavior
fragments/snippets/     reusable commands/workflows
fragments/notes/        unclassified knowledge; promote periodically
fragments/bugs/         open bugs or memorable scars
fragments/feedback/     user corrections; fold into preferences over time
fragments/credentials/  credential locations and handling rules only
```

Credential rule: active memory should store **where** to find secrets and how to handle them, not raw secret values.

Backup-before-delete convention for deep cleanup:

```text
/home/agent/artifacts/memory-consolidation/YYYY-MM-DD/<cluster>.json
```

Write source rows there before deleting a group of superseded fragments.

## Behavioral insights

Fragments store facts. Insights store reusable behavioral rules.

Why care? Because facts tell the agent **what is true**, while insights tell it **how to behave next time**.

A fragment is something like:
- "Use the staging Redis instance at `redis://staging.internal:6379/2`."

An insight is something like:
- "When debugging staging issues, check Redis connectivity before blaming Sidekiq."

Or:
- fragment: "The docs site is built with Hugo and deployed from GitHub Actions."
- insight: "For docs changes, preview locally first and verify mobile layout before opening the PR."

Another invented example:
- fragment: "This repo keeps generated files under `internal/embed/`."
- insight: "When changing frontend assets, search for the generated embed step before assuming the file is served directly."

That is the point of insights: they capture **reusable judgment**, not just stored facts.

```bash
term-llm memory insights list --agent assistant
term-llm memory insights search "code review"
term-llm memory insights expand "debugging deploy failures" --agent assistant
```

You can add or reinforce them manually:

```bash
term-llm memory insights add --agent assistant \
  --category workflow \
  --confidence 0.8 \
  --content "Write code first, then wait for approval before starting services."

term-llm memory insights reinforce 42 --agent assistant
```

Good insight candidates usually sound like:
- a rule of thumb
- a repeated correction
- a workflow preference
- a pattern that prevents future mistakes

Bad insight candidates are usually just facts wearing a fake mustache.

High-confidence insights are meant to shape future behavior, not just sit in storage looking profound.

## Status and health checks

```bash
term-llm memory status
```

That gives you, per agent:

- fragment count
- last mined time
- how many completed sessions are still pending mining

## Storage model

The memory database defaults to:

```text
~/.local/share/term-llm/memory.db
```

You can override it per command:

```bash
term-llm memory status --db /tmp/memory.db
```

Or via environment:

```bash
export TERM_LLM_MEMORY_DB=/path/to/memory.db
```

## When to use sessions vs memory

Use **sessions** when you want:

- resumable conversation state
- transcript search
- exports and inspection

Use **memory** when you want:

- durable facts across sessions
- stable preferences and conventions
- behavior-shaping insights
- compressed summaries that survive beyond one chat
