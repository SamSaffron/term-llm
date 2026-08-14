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

### Audit CLI provider wire traffic

`claude-bin`, `grok-bin`, `cursor-bin`, and `agy-bin` delegate authenticated requests to local CLI binaries whose behavior can change independently of term-llm. Set `TERM_LLM_CLI_WIRE_TRACE` to enable an authenticated, loopback-only TLS audit proxy and capture the decrypted traffic sent by those binaries:

```bash
TRACE_ROOT="$(mktemp -d /tmp/term-llm-wire.XXXXXX)"
chmod 700 "$TRACE_ROOT"

TERM_LLM_CLI_WIRE_TRACE="$TRACE_ROOT" \
  term-llm --no-session chat \
    --provider claude-bin:haiku \
    --skills none --no-search --text \
    --auto-send "Wire audit turn one: remember marker AUDIT_ALPHA." \
    --auto-send "Wire audit turn two: repeat AUDIT_ALPHA, then say AUDIT_BETA."
```

The environment variable names a **root directory**. Every audited CLI process creates a separate private run directory beneath it, so one command may produce multiple runs. Claude and Cursor normally start one CLI process per provider turn; Grok's resident ACP process can span multiple turns. Model-listing subprocesses use provider names ending in `-models`.

Each run contains:

```text
<timestamp>-<pid>-<provider>-<random>/
├── events.jsonl
└── connections/
    ├── 000001-<host>-request.bin
    ├── 000001-<host>-response.bin
    └── ...
```

`events.jsonl` maps connection IDs and destinations to capture files and records negotiated ALPN, byte counts, errors, and shutdown. Request and response files contain the exact HTTP plaintext observed after TLS termination. They are not packet captures: TLS records are removed, and the proxy may constrain ALPN to HTTP/1.1 for compatibility.

Useful first-pass inspection commands:

```bash
find "$TRACE_ROOT" -name events.jsonl -print -exec jq -c . {} \;
find "$TRACE_ROOT" -name '*-request.bin' -print
rg -a 'AUDIT_ALPHA|tools|system|messages|input' "$TRACE_ROOT"
```

Claude and Grok generation calls are generally readable HTTP/1.1 JSON. Cursor's generation channel uses HTTP/2 with Connect/protobuf envelopes. The audit proxy automatically extracts each HTTP/2 request DATA stream, decompresses gzip-marked Connect messages, and writes additional files named like:

```text
000001-agentn.global.api5.cursor.sh-request-stream-1-message-001.bin
```

Those files preserve the decoded protobuf bytes without rewriting them; `strings`, `rg -a`, or a protobuf wire decoder can inspect prompt, tool, skill, path, and continuation markers. The original HTTP/2 stream remains alongside them.

`agy-bin` retains its mandatory native-tool filtering proxy. In audit mode that proxy is chained through the wire proxy, and its CA bundle trusts both ephemeral authorities. This preserves agy's fail-closed tool filtering while allowing the forwarded traffic and all other agy HTTPS connections to be captured. The specialized `TERM_LLM_AGY_PROXY_TRACE_FILE` described below remains useful when you specifically need agy's original request next to term-llm's filtered request.

The audit proxy overrides inherited `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, and `NODE_EXTRA_CA_CERTS` **only in the audited CLI child**. Localhost remains excluded so CLI-to-term-llm MCP traffic does not loop through the proxy. An existing `SSL_CERT_FILE` is included in the generated bundle so private enterprise roots continue to work. The trace-root variable itself is removed from the child environment.

> **Treat the entire trace directory as secret material.** Captures are intentionally unredacted and can contain complete prompts, tool schemas and results, source code, account metadata, cookies, OAuth/API authorization headers, and provider responses. Run directories are created with mode `0700`; files use mode `0600`; symlink roots are rejected; proxy credentials are random; the CA private key exists only in memory; and temporary CA files are removed during normal shutdown. Use the mode only for short diagnostics, do not attach raw traces to public issues, and delete the directory when finished:
>
> ```bash
> rm -rf -- "$TRACE_ROOT"
> ```

A successful provider response proves that the observed request passed through the proxy, but this mode is an audit recorder rather than an OS network sandbox. It does not prevent a binary from opening an additional direct socket through a transport that ignores proxy environment variables. Compare expected generation requests with all destinations in `events.jsonl`, and investigate successful turns with no matching generation capture.

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
