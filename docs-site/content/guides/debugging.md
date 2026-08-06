---
title: "Debugging"
weight: 2
description: "Use provider debug output and debug logs to figure out what the runtime is actually doing."
kicker: "Troubleshooting"
source_readme_heading: "Debugging"
next:
  label: Configuration reference
  url: /reference/configuration/
---
Use `--debug` to print provider-level diagnostics (requests, model info, etc.). Use `--debug-raw` for a timestamped, raw view of tool calls, tool results, and reconstructed requests. Raw debug is most useful for troubleshooting tool calling and search.

### Exercise automatic compaction locally

The `debug:compaction` provider is a hermetic, zero-cost TUI scenario with a 20k context window. Its deterministic first-turn usage crosses the default compaction threshold; the next turn emits a short internal brief and continues from the compacted context. Start it, send any seed message, then send a second message:

```bash
term-llm chat --provider debug:compaction
```

The second turn's status line should progress through the compaction phases. Normal chat should retain a `Context compacted` boundary immediately before the continuation; `Ctrl+O` shows the summary and retained-tail details. No provider credentials, local model, or special configuration are required.

To exercise tool ordering across the boundary in one agent turn, enable the harmless `glob` tool and send exactly `run the tool compaction probe`:

```bash
term-llm chat --provider debug:compaction --tools glob
```

The deterministic sequence is three `glob` calls, automatic compaction, three more `glob` calls, then a completion message. This is intended for live TUI recordings and regression tests of tool/compaction placement.

For a non-interactive smoke test, use:

```bash
term-llm chat --provider debug:compaction \
  --auto-send "seed the compaction probe" \
  --auto-send "continue after compaction"
```

### Trace agy-bin generation requests

To inspect the exact generation payload that `agy` sends through term-llm's compatibility proxy, set `TERM_LLM_AGY_PROXY_TRACE_FILE` to a JSONL file in a private directory:

```bash
mkdir -m 700 /tmp/term-llm-agy-trace
TERM_LLM_AGY_PROXY_TRACE_FILE=/tmp/term-llm-agy-trace/requests.jsonl term-llm chat
rg 'output\.txt|brain/' /tmp/term-llm-agy-trace/requests.jsonl
```

Each record contains both `original_request` from agy and `forwarded_request` after term-llm rehydrates agy's private spill artifacts and removes native tools. The trace writer rejects symlink targets and creates the file with mode `0600`. It contains complete prompts and conversation content, so enable it only while diagnosing a problem and delete it afterward.

### Debug Logging

term-llm maintains debug logs for troubleshooting. Use the `debug-log` command to view and manage them:

```bash
term-llm debug-log                           # Show recent logs
term-llm debug-log list                      # List available log files
term-llm debug-log show [file]               # Show a specific log file
term-llm debug-log tail                      # Show last N lines
term-llm debug-log tail --follow             # Follow logs in real-time
term-llm debug-log search "pattern"          # Search logs for a pattern
term-llm debug-log clean                     # Clean old log files
term-llm debug-log clean --days 7            # Keep only last 7 days
term-llm debug-log export --json             # Export logs as JSON
term-llm debug-log enable                    # Enable debug logging
term-llm debug-log disable                   # Disable debug logging
term-llm debug-log status                    # Show logging status
term-llm debug-log path                      # Print log directory path
```

**Key flags:**
| Flag | Description |
|------|-------------|
| `--days N` | Limit to logs from last N days |
| `--show-tools` | Include tool calls/results in output |
| `--raw` | Show raw log entries without formatting |
| `--json` | Output as JSON |
| `--follow` | Follow logs in real-time (with tail) |
