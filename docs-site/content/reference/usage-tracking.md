---
title: "Usage tracking"
weight: 4
description: "Inspect token usage and local cost data for term-llm, Claude Code, and Gemini CLI."
featured: true
kicker: "Accounting"
source_readme_heading: "Usage Tracking"
next:
  label: Session management
  url: /reference/sessions/
---
Use the `usage` command to inspect token consumption and local cost data across supported tools:

```bash
term-llm usage                           # Show all usage
term-llm usage --provider claude-code    # Filter by provider
term-llm usage --provider term-llm       # term-llm usage only
term-llm usage --since 20250101          # From specific date
term-llm usage --breakdown               # Per-model breakdown
term-llm usage --json                    # JSON output
```

Supported sources: Claude Code, Gemini CLI, and term-llm's own usage logs.

## What it covers

Supported sources:

- term-llm
- Claude Code
- Gemini CLI

Useful patterns:

```bash
term-llm usage --provider term-llm --breakdown
term-llm usage --provider claude-code --since 20250101
term-llm usage --json
```
