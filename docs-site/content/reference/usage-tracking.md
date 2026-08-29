---
title: "Usage tracking"
weight: 4
description: "Inspect local token costs and live provider subscription limits across supported providers."
featured: true
kicker: "Accounting"
source_readme_heading: "Usage Tracking"
next:
  label: Session management
  url: /reference/sessions/
---
Use the `usage` command to inspect local token consumption and current provider subscription limits:

```bash
term-llm usage                           # Show local usage from the last seven days
term-llm usage --provider claude-code    # Filter local Claude Code history
term-llm usage --provider term-llm       # Filter local term-llm history
term-llm usage --provider agy-bin        # Fetch live Antigravity model quotas
term-llm usage --provider claude-bin     # Fetch live Claude subscription limits
term-llm usage --provider cursor-bin     # Fetch live Cursor plan usage
term-llm usage --provider chatgpt        # Fetch live ChatGPT Codex limits
term-llm usage --provider grok           # Fetch live Grok coding-credit usage
term-llm usage --provider opencode-go    # Fetch live OpenCode Go plan usage
term-llm usage --since 20250101          # Filter local usage by date
term-llm usage --breakdown               # Show a local per-model breakdown
term-llm usage --json                    # Emit structured JSON
```

## Local usage

Local history and estimated cost reporting are available for:

- term-llm
- Claude Code

Date, breakdown, and external-usage filters apply to these local records.

## Live subscription usage

The `agy-bin`, `chatgpt`, `grok`, `claude-bin`, `cursor-bin`, and `opencode-go`
providers fetch current account limits without invoking a model or consuming
tokens. `copilot` fetches AI Credit usage from GitHub's billing API.

For Grok, authenticate once and then query the current weekly or monthly
coding-credit period:

```bash
term-llm auth login grok
term-llm usage --provider grok
term-llm usage --provider grok --json
```

Antigravity usage requires `agy` 1.1.11 or newer, installed and logged in. The
integration runs agy's built-in read-only print command:

```bash
agy --output-format json -p=/usage
```

That command refreshes quota and returns structured weekly and five-hour model
limits with zero agent turns and zero tokens. term-llm checks the agy version
before invoking it so older releases cannot accidentally send `/usage` to a
model.

For direct live providers (`chatgpt`, `grok`, and `opencode-go`), `--since`,
`--until`, `--breakdown`, and `--include-external` are not supported. The
CLI-backed `agy-bin`, `claude-bin`, and `cursor-bin` reports support only
`--json`; Copilot has its own billing-period and scope filters.
