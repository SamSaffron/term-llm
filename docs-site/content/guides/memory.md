---
title: "Memory"
weight: 9
featured: true
description: "Mine durable facts from sessions, search fragments, manage insights, and understand how memory differs from sessions."
kicker: "Long-term memory"
next:
  label: Agents
  url: /guides/agents/
---
## What memory is for

term-llm has a long-term memory system separate from normal chat sessions.

- **Sessions** store conversation history so you can resume, inspect, export, or search a chat.
- **Memory** stores durable fragments and behavioral insights mined from your sessions.

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

## Mine sessions into fragments

```bash
term-llm memory mine
```

This scans completed sessions and ongoing conversations, extracts durable memory, and stores it as fragments in the memory database. Web and chat-platform conversations stay active between turns, so they are included too. Each session is mined from where the last run stopped, so new turns are picked up without rereading earlier ones. An ongoing conversation is read only once it has been quiet for 10 minutes, so a reply that is still being written is never mined half-finished.

Useful flags:

```bash
term-llm memory mine --since 24h
# Message-time scope: inclusive lower bound, exclusive upper bound.
term-llm memory mine --message-since 2026-01-01T00:00:00Z --message-before 2026-01-22T00:00:00Z --insights=false
term-llm memory mine --limit 20
term-llm memory mine --include-subagents
term-llm memory mine --dry-run
term-llm memory mine --embed=false
term-llm memory mine --insights
```

`--since` still filters **session update time**, not message dates. Use
`--message-since` and/or `--message-before` (RFC3339 timestamps with timezone)
when evidence itself must be time-bounded. The lower bound is inclusive and
intentionally skips older messages, advancing the raw sequence checkpoint; these
skips are counted explicitly, **not recorded as mined sources**. Choose the lower
bound before mining: widening it later cannot recover evidence already passed by
an existing checkpoint. Do not reset checkpoints to work around that.

The exclusive upper bound is a **sequence barrier**: its message and every later
sequence stay unread. Timestamps need not be ordered; later sequences may contain
older in-window evidence, so an upper-bound stop is not proof that all in-window
messages behind it were examined. A later invocation without that upper bound can
resume safely. Missing/zero timestamps fail closed without advancing that batch.
Existing checkpoints are never rewound. Active-session quiet-period and child
exclusions still apply.

Message-bounded mode never clips messages to fit the initial prompt budget. It
ends a batch before a message that would overflow; a single oversized message
returns an actionable error with its checkpoint unchanged, so increase the budget
rather than silently losing its tail. Empty/role-only/out-of-window batches need
no model call. Source links identify only actual supplied sequence spans, not the
whole traversed checkpoint interval. `--stats` also reports usage on failed
extractions when the provider supplied it; missing usage is unknown, not zero.

Message bounds apply to session fragment extraction. Image mining is skipped
without changing its watermark, and combining bounds with `--insights` is rejected.
Embedding and promotion remain separate phases; use `--embed=false --promote never`
for an extraction-only catchup. Lookup results can still consult existing memory;
message bounds do not claim to date-filter the memory database itself.

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

### Native content protection

Automated memory processing uses native secret heuristics with protective defaults
and complete **trusted owner control** in `config.yaml`. There is no executable,
shell hook, plugin, environment override, or model-selected policy. Missing policy
and empty lists mean all built-ins enabled, no extra rules, and no paths excluded.

```yaml
memory:
  secrets:
    disable: []     # detector names, or [all] to disable every built-in
    patterns: []    # extra RE2 patterns; only matched spans are redacted
  exclude_paths: [] # fragment globs, e.g. ['fragments/credentials/**']
```

- Disable a noisy heuristic with `disable: [named-credential]` or `[cli-user]`.
- Extend defaults with `patterns: ['synthetic_private_[A-Z0-9]+']`.
- Replace defaults with `disable: [all]` plus your patterns.
- Opt out entirely with `disable: [all]` and no patterns. This is intentional owner
  control, not a safety bug; detected-secret checks and redaction are then off.
- Exclude selected fragment paths using glob data, including `**` directories.
  No taxonomy room or deployment-specific file name is excluded by default;
  credential-location and handling-rule memories remain legitimate searchable facts.

[Detector names and source-backed patterns](/reference/configuration/#memory-secret-detectors)
are documented so a noisy rule can be disabled and replaced without patching Go.
Names are stable; unknown detector names/options and invalid patterns/globs fail
before provider work. Custom expressions use Go/RE2 and must not match the empty
string. Pattern errors identify the index, never the literal. Pattern config is
sensitive in config display. There are no arbitrary rule-count or rule-length caps.
The former `memory.secret_patterns` spelling was unreleased and is replaced—not
aliased—by `memory.secrets.patterns`.

#### Redaction, retries and checkpoints

Every enabled detector examines the **original complete text**; overlapping spans
are unioned and replaced once. A match in one section of existing `recent.md` does
not erase other sections. Complete source text is sanitized before clipping or
provider calls, including tool metadata, taxonomy, lookup sources, insights and
embedding input. Search results hydrate complete source text before excerpting:
redacting an already-clipped FTS snippet can miss a credential split at the edge.
System, developer and event roles are not conversational mining evidence.

Model-proposed writes are checked before persistence. Rejections identify detector
names or custom-rule indices, not values. Normal path/empty/size mistakes remain
ordinary actionable tool errors. Tools execute serially; a successful corrected
write to the **same canonical path** resolves its prior failure. Other accepted
writes do not clear unresolved evidence. An extraction finishing with unresolved
writes (or a provider failure) keeps that batch's cursor, warns and returns an
incomplete result after unrelated sessions and later phases have run. It does
**not** blindly advance past rejected evidence. A completed extraction with no new
memory is an examined outcome and may advance normally. Explicit owner path
exclusions are counted skips, not hidden rejected writes.

Already accepted partial writes are not rolled back; their source attribution is
attempted even if the batch remains incomplete (DB attribution failures still
warn). Counts include rejected attempts and accepted corrections. Changing owner
policy or retrying corrected content does not require resetting live cursors.

Images are deterministic source input: sanitize metadata spans, write the projected
fragment and continue. Row failures do not block other images/phases. The image
watermark advances only through fully processed timestamp groups before the first
failure. Its boundary group is replayed inclusively/idempotently, so a late row
sharing SQLite's timestamp is not lost. Later accepted rows may be replayed while
an earlier failed row is retried. Excluded and failed rows are reported explicitly.

Promotion and `update-recent` keep atomic replacement and checkpoints intact on
failure. They require nonempty valid UTF-8 within budget and no detected secret;
headings, code fences and editorial/refusal wording are not hard security rules.
One compaction request is allowed for oversized output; a still-oversized result
returns a size error without truncation or replacement. Native validation does
not guarantee useful content, factual truth, freshness or faithful LLM retention.

#### Diagnostics and limits

Provider errors retain useful **sanitized** details. A sanitized error never keeps
an unchecked provider body in its unwrap chain; cancellation identity is retained.
With detectors enabled, raw/structured model logs remain suppressed because output
can contain unsafe generated text even when input was sanitized. `--debug-raw`
warns explicitly about this; it is passed through only with owner full optout
(`disable: [all]`, no extra patterns). Raw optout diagnostics can expose secrets.
Memory policy does not change the existing live-preview redaction behavior.

These are heuristics, not universal DLP or a prompt-injection firewall. Unknown,
encoded, fragmented or paraphrased secrets can evade recognition; default rules
can over-redact location prose (for example `Password: kept in the team vault`).
An owner can turn that detector off or replace it with a narrower rule. Existing
stored sources are not scrubbed. Manual memory administration and unrelated
runtime tools remain outside this automated-pipeline policy.

Dry-run still makes model calls. `mine --since` selects session update timestamps,
not message dates; it is not a strict calendar bound for historical backfill.

## Consolidate safely

```bash
term-llm memory consolidate --agent assistant
term-llm memory consolidate --agent assistant --apply
```

`consolidate` is the safer current-state workflow. It defaults to a dry run, prints the proposed `recent.md` rewrite, and shows a non-destructive decay preview. `--apply` writes `recent.md`; it does not delete fragments, move fragments, perform full palace cleanup, or rewrite stored decay scores.

Use it when you want to compact recent working memory without treating age as permission to forget.

### Cadence

A practical schedule is:

- run `memory mine` frequently, such as every 30 minutes, to extract durable candidates from recent sessions
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
- how many sessions still have messages pending mining

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
