---
title: "Usage tracking"
weight: 4
description: "Inspect local token costs and live subscription limits across supported providers."
featured: true
kicker: "Accounting"
source_readme_heading: "Usage Tracking"
next:
  label: Session management
  url: /reference/sessions/
---
Use the `usage` command to inspect local token consumption and live provider limits:

```bash
term-llm usage                           # Show local usage from the last seven days
term-llm usage --provider agy-bin        # Live Antigravity model quotas
term-llm usage --provider claude-bin     # Live Claude subscription limits
term-llm usage --provider cursor-bin     # Live Cursor plan usage
term-llm usage --provider chatgpt        # Live ChatGPT Codex plan usage
term-llm usage --provider opencode-go    # Live OpenCode Go plan usage
term-llm usage --provider claude-code    # Local Claude Code history
term-llm usage --provider term-llm       # Local term-llm usage
term-llm usage --since 20250101          # Local usage from a specific date
term-llm usage --breakdown               # Local per-model breakdown
term-llm usage --json                    # Machine-readable output
```

## Live subscription limits

`agy-bin`, `chatgpt`, `claude-bin`, `cursor-bin`, and `opencode-go` return the
current account limits without invoking a model. Live providers support
`--json`, but not local-history filters such as `--since` or `--breakdown`.

Antigravity usage requires `agy` 1.1.11 or newer, installed and logged in. The
integration runs agy's built-in read-only print command:

```bash
agy --output-format json -p=/usage
```

That command refreshes quota and returns structured weekly and five-hour model
limits with zero agent turns and zero tokens. term-llm checks the agy version
before invoking it so older releases cannot accidentally send `/usage` to a
model.

## Local usage history

Local history and estimated cost reporting currently cover:

- term-llm
- Claude Code

Useful patterns:

```bash
term-llm usage --provider term-llm --breakdown
term-llm usage --provider claude-code --since 20250101
term-llm usage --json
```
