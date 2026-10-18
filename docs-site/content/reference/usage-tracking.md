---
title: "Usage tracking"
weight: 4
description: "Inspect local token costs, OpenRouter account analytics, and live provider subscription limits."
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

## OpenRouter account usage

```bash
term-llm usage -p openrouter                   # Model breakdown, last 30 UTC days including today
term-llm usage -p openrouter --days 7
term-llm usage -p openrouter --daily            # Daily totals instead of model rows
term-llm usage -p openrouter --since 20261001 --until 20261015
term-llm usage -p openrouter --json
```

OpenRouter reporting uses the read-only analytics query API, without invoking a
model. It reports **account-wide** activity, including calls outside term-llm,
not just local history. Models are sorted by reported cost, highest first. The
current account balance is a separate snapshot, not the balance at the end of a
historical report. `usage` without `-p openrouter` (including `-p all`) continues
to read local records only.

### Credentials

For per-model and daily analytics, configure an OpenRouter **management key**:

```yaml
providers:
  openrouter:
    api_key: ${OPENROUTER_API_KEY}              # Regular inference key; optional for analytics
    management_key: ${OPENROUTER_MANAGEMENT_KEY}
```

Alternatively, export `OPENROUTER_MANAGEMENT_KEY` without adding the setting.
An explicit configuration value takes precedence over the environment fallback.
Like `api_key`, `management_key` supports environment references, `file://`,
`op://`, and command references. It is resolved only when requesting OpenRouter
usage, never during model inference. A management key alone is sufficient for
account analytics and balance. Keep using a regular, least-privilege API key for
model requests.

**Management keys are privileged credentials**, even though this command only
makes read-only reporting requests. Keep them private; do not commit literal
keys to a repository. See OpenRouter's
[management key documentation](https://openrouter.ai/docs/guides/overview/auth/management-api-keys).

### Dates and accounting

- The default is **30 UTC calendar days**, from midnight 29 days ago through now.
  Today is included and is partial. `--days N` changes this window (1–3660;
  this is a client safety bound, not a promise about API data retention).
- `--since` and `--until` accept `YYYYMMDD`. Both dates are inclusive in the CLI;
  JSON represents the range as an inclusive `since` and **exclusive** `until`.
  An end date of today is capped at the report's captured current time.
- `--since` alone ends now. `--until` alone selects 30 calendar days ending on
  that date. Future dates, reversed ranges, and combining an explicit `--days`
  with either date flag are rejected.
- Costs are OpenRouter's reported `total_usage`, in USD—not estimates from a
  pricing catalogue. BYOK usage, credit usage, and BYOK fees are exposed
  separately in JSON; term-llm does not invent an additive relationship between
  those values.
- Terminal request and token counts use compact notation such as `120k`,
  `12.78M`, and `1.2B`. JSON retains exact integer counts.
- **Cache %** is cached input tokens divided by input tokens, shown for each
  model, day, and total. Totals are token-weighted, not averages of percentages.
  A zero-input row shows `—`; JSON uses `cache_hit_percent: null`. The ratio is
  derived from the reported counters without clamping; inconsistent upstream
  counters can therefore produce a value above 100%, rather than hiding the
  anomaly. Cached and reasoning tokens are detail counters, not additional
  tokens to add on top of input and output. Reporting can lag, and a successful
  query does not guarantee historical retention for every requested date.

The analytics query API includes today. OpenRouter's separate `/activity`
endpoint only covers completed UTC days, so term-llm does **not** use that as a
fallback for this report.

`--days` and `--daily` are OpenRouter-only. `--breakdown` is accepted but redundant
because the default already shows models. `--include-external` and Copilot
billing flags do not apply.

### Fallback and JSON

Without a management key, or if analytics fails, the command tries to show the
current account balance and the regular API key's summary counters. Such a
report prominently states that the **requested-period breakdown is unavailable**.
The key's daily, weekly, monthly, and all-time counters are scoped to **that key**,
not the account, and are not substitutes for the requested N-day totals.
Management-key counters are not shown as inference-key usage. There is no silent
fallback to local history.

`--json` always emits the same versioned report shape, regardless of `--daily`:

- `schema_version`, `provider`, `source`, `scope`, `generated_at`, `period`, and
  `currency` identify the report and UTC boundaries.
- `status: "complete"` means analytics was validated and not truncated. It is
  not a guarantee of real-time freshness or historical retention.
- `status: "summary_only"` means the requested analytics is unavailable. `totals`
  is `null`, and `models` and `daily` are empty arrays—not zero-spend claims.
- `models` and `daily` are both included for a complete report, even when the
  terminal view shows only one. `totals` contains reported request, token, and
  cost aggregates. A valid empty response has zero totals and empty arrays,
  described as “No activity returned for this period.”
- `balance` and `key_usage` are nullable independent components. `key_usage.scope`
  is `"key"`; account analytics and balance are account-scoped. `cached_at`, when
  available, identifies the API's cached snapshot time.
- `warnings` explains missing credentials, unavailable components, or degraded
  results. Missing/unknown components are not represented as zero.

A useful summary fallback exits successfully. Scripts requiring model history
should check `status == "complete"`, not only the exit code. If no useful
reporting component can be fetched, or the command is canceled, it returns an
error. Invalid, malformed, or truncated analytics is not presented as a complete
report. If a high-volume query is truncated, try a narrower range with `--days`;
term-llm does not silently display partial totals. Optional balance/key-summary
failures do not discard good analytics.

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
