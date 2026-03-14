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
term-llm memory insights list --agent jarvis
```

Use `--agent` when you want to scope memory to a particular agent:

```bash
term-llm memory status --agent jarvis
term-llm memory search "docs tone" --agent jarvis
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
term-llm memory fragments list --agent jarvis --limit 20
term-llm memory fragments list --filter-path preferences
```

Show a fragment by numeric ID or path:

```bash
term-llm memory fragments show 42 --agent jarvis
term-llm memory fragments show fragments/preferences/editor.md --agent jarvis
```

Show where a fragment came from:

```bash
term-llm memory fragments sources 42 --json
```

Manage fragments manually when you need precise control:

```bash
term-llm memory fragments add fragments/preferences/search.md --agent jarvis --content "Prefer Exa with Brave fallback."
term-llm memory fragments update fragments/preferences/search.md --agent jarvis --content "Prefer Exa with Brave as fallback only."
term-llm memory fragments delete fragments/preferences/search.md --agent jarvis
```

## Promote recent fragments into an agent summary

```bash
term-llm memory promote --agent jarvis
```

Promotion condenses recently changed fragments into an agent-level `recent.md` summary. It is a compression step, not a raw dump.

Useful flags:

```bash
term-llm memory promote --agent jarvis --since 6h
term-llm memory promote --agent jarvis --recent-max-bytes 20000
term-llm memory promote --agent jarvis --dry-run
```

## Behavioral insights

Fragments store facts. Insights store reusable behavioral rules.

```bash
term-llm memory insights list --agent jarvis
term-llm memory insights search "code review"
term-llm memory insights expand "debugging deploy failures" --agent jarvis
```

You can add or reinforce them manually:

```bash
term-llm memory insights add --agent jarvis \
  --category workflow \
  --confidence 0.8 \
  --content "Write code first, then wait for approval before starting services."

term-llm memory insights reinforce 42 --agent jarvis
```

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
