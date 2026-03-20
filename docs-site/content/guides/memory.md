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
- reranking and decay-aware scoring

Useful flags:

```bash
term-llm memory search "provider routing" --limit 10
term-llm memory search "deploy key" --bm25-only
term-llm memory search "session persistence" --no-decay
term-llm memory search "embedding provider" --embed-provider gemini
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
