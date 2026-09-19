---
title: "Debugging"
weight: 2
description: "Check a local install with term-llm doctor, then use provider debug output, wire traces, and debug logs to figure out what the runtime is actually doing."
kicker: "Troubleshooting"
source_readme_heading: "Debugging"
next:
  label: Configuration reference
  url: /reference/configuration/
---
Use `--debug` to print provider-level diagnostics (requests, model info, etc.). Use `--debug-raw` for a timestamped, raw view of tool calls, tool results, and reconstructed requests. Raw debug is most useful for troubleshooting tool calling and search.

## Check the install with `term-llm doctor`

`term-llm doctor` inspects a local install and reports what is broken, ignored, or left over. Every check is offline and read-only by default; repairs run only with `--fix`, prompt individually unless `--yes` is given, and back up whatever they rewrite.

```bash
term-llm doctor                 # report problems
term-llm doctor -v              # include checks that passed
term-llm doctor --json          # machine-readable findings
term-llm doctor --only db,stale # run a subset
term-llm doctor --fix           # repair, prompting for each finding
term-llm doctor --fix --yes     # repair without prompting
```

Findings carry a severity of `ok`, `info`, `warn`, or `error`. The command exits non-zero only when something reached `error`.

### Reading the report

Each check prints one status line — `✓` healthy, `!` degraded, `✗` broken — followed by its findings, with healthy findings hidden unless `-v` is given. On a terminal the status line is preceded by an animated `checking …` line that the result overwrites, so a slow check (a large `sessions.db` integrity scan, a `$()` config value shelling out) shows progress instead of a frozen prompt. Piped output, `--json`, `CI=1`, and `TERM=dumb` skip the animation and the colors; the body is identical either way.

### What it checks

#### `db` — databases

For every SQLite database term-llm owns (`sessions.db`, `memory.db`, `jobs_v2.db`, `file_history.db`, `file_observations.db`, `hub/attention.db`) the check:

- runs `PRAGMA integrity_check` and `PRAGMA foreign_key_check`;
- migrates a **fresh** database in a temporary directory and diffs its schema signature against the user's file. This is the golden-schema comparison: it catches a change that edited the canonical `CREATE TABLE` without adding a migration, which reaches new installs but never runs on existing ones;
- compares each external-content FTS5 index against its source table using the index's `_docsize` shadow table. Drift makes search silently return incomplete results, and is repairable with `--fix` (`INSERT INTO <fts>(<fts>) VALUES('rebuild')`).

Missing objects are errors; leftover objects are warnings. Columns, indexes, triggers, and foreign keys of a table that is missing entirely are folded into the table entry so the report stays readable.

#### `config` — configuration

- Keys the loader ignores, reported per key with a Levenshtein suggestion. `--fix` comments the whole block out in place, preserving every other byte including comments, after copying the file to `config.yaml.doctor-bak-<timestamp>`.
- The deprecated `provider` alias for `default_provider`.
- Per declared provider: legacy field combinations that are rejected as incompatible, deferred values (`op://`, `srv://`, `file://`, `$()`) that fail or resolve empty, `${VAR}` references to unset variables, and providers with no resolvable credential.
- A `default_provider` that is neither configured nor built in.

Only providers the config *file* declares are examined, plus the default provider. Loaded config always contains every built-in provider because defaults populate the map, so iterating it would lecture about providers you never set up.

Deferred value resolution executes the same code the loader runs lazily, including `$()` commands. Use `--skip-deferred` to suppress it.

#### `credentials` — stored OAuth credentials

Offline checks over `chatgpt_oauth.json`, `grok_oauth.json`, and `copilot_oauth.json`: file permissions (`--fix` restores `0600`), parse failures, missing access tokens, and expiry. An expired token with a refresh token is informational because the next request renews it; an expired token without one needs `term-llm auth login`.

#### `mcp` — MCP servers

Parses `mcp.json` without starting anything: invalid JSON, invalid server definitions, stdio commands that are not on `PATH`, non-HTTP URLs, and duplicate server names. Duplicates matter because the loader decodes into a map, so a shadowed definition disappears silently.

#### `stale` — leftover files and rows

- `-wal`/`-shm` files whose database is gone (`--fix` deletes them).
- Write-ahead logs past the warning threshold (`--fix` runs `PRAGMA wal_checkpoint(TRUNCATE)`).
- Service `install.lock` files that no process holds.
- Orphaned rows: messages, workspace grants, and queued push notifications in `sessions.db`; runs and run events in `jobs_v2.db`. `--fix` deletes them.

#### `binaries` — external commands

Reports `git`, `sh`, `rg`, `ffprobe`, and `node`, plus the CLI required by each configured subprocess provider (`claude`, `grok`, `cursor-agent`, `agy`). Each missing command explains what stops working.

## Exercise automatic compaction locally

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

## Audit CLI provider wire traffic

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

## Trace agy-bin generation requests

To inspect the exact generation payload that `agy` sends through term-llm's compatibility proxy, set `TERM_LLM_AGY_PROXY_TRACE_FILE` to a JSONL file in a private directory:

```bash
mkdir -m 700 /tmp/term-llm-agy-trace
TERM_LLM_AGY_PROXY_TRACE_FILE=/tmp/term-llm-agy-trace/requests.jsonl term-llm chat --provider agy-bin
rg 'output\.txt|brain/' /tmp/term-llm-agy-trace/requests.jsonl
```

Each record contains both `original_request` from agy and `forwarded_request` after term-llm rehydrates agy's private spill artifacts and removes native tools. The trace writer rejects symlink targets and creates the file with mode `0600`. It contains complete prompts and conversation content, so enable it only while diagnosing a problem and delete it afterward.

## Live voice diagnostics

These apply to [live voice](/guides/live-voice/). Diagnostics are completely off by default. To investigate Gemini audio gaps, start the server explicitly with metadata diagnostics:

```sh
term-llm serve web --debug
```

`--debug` logs Gemini setup timing, incoming event kinds and inter-event timing, input/interim/output transcription counts, PCM chunk byte sizes and cumulative counts, turn/interruption boundaries, and connection errors/close reasons. It does **not** log PCM base64, prompts, seeded history, authentication values, media capabilities, or API keys. Server diagnostics are written to the `term-llm serve` process's stderr; keep that terminal open or inspect the service manager's captured stderr.

For sanitized inbound Gemini event JSON as well, use the global raw-debug flag (it also enables the metadata diagnostics):

```sh
term-llm serve web --debug-raw
```

You can instead enable diagnostics only for live sessions using an environment variable (preserve your usual server/Hub arguments):

```sh
TERM_LLM_LIVE_DEBUG=metadata term-llm serve web
TERM_LLM_LIVE_DEBUG=raw term-llm serve web
```

`1`, `true`, `on`, and `metadata` enable metadata diagnostics; `raw` also enables sanitized raw events. Values are case-insensitive and whitespace is ignored. Unset, `0`, `false`, `off`, and unrecognized values do not enable diagnostics. Existing `--debug` / `--debug-raw` flags remain additive and are not disabled by the variable. Set it in the node process's environment and restart the node; no config edit or Hub restart is needed.

**Raw diagnostics are sensitive:** sanitized raw events may include input and output transcript text. PCM payloads are replaced by decoded byte lengths, tool arguments and prompt/history containers are redacted, and keys, authorization values, bearer tokens, media capability tokens, and session-resumption handles are redacted. Do not publish a raw log without reviewing it. Provider and browser reports carry the same `live_id` for correlation.

While either flag or the environment opt-in is active, open the browser's developer tools and select **Console**. Lines prefixed with `[live]` report the Web Audio context state and a five-second cumulative batch of packets received, enqueued into the output worklet (`scheduled`), and actually consumed (`ended`), plus FIFO playback seconds, worklet underrun episodes, interruption flush counts (the legacy buffer-reset counter stays zero), microphone queue drops, and input POST latency/inflight counts. Worklet consumption acknowledgements are aggregated rather than emitted for every render quantum. Output/input silence ages are observations only: ordinary silence on an open stream does not fail or restart a healthy call. The same fixed numeric report is posted through the authenticated, per-call `/v1/live/sessions/{live_id}/diagnostics` route and appears in server stderr as `[live browser]`.

The reporting route is unavailable unless live diagnostics are enabled, accepts no arbitrary browser messages or URLs, is size/rate/lifetime bounded, and stops with the call. With neither flags nor the environment opt-in enabled, the response has no diagnostic opt-in, so the browser creates no diagnostic timer and makes no diagnostic requests.

### Capture PCM artifacts

When investigating a beep or other rendering artifact on the Gemini PCM transport, you can additionally capture the exact provider PCM received by the browser and the output of the browser's Web Audio playback graph. This is a second, explicit browser opt-in and works only when the server start response has diagnostics enabled as described above. In DevTools **Console**, run:

```js
localStorage.setItem("term-llm-live-pcm-capture", "1");
```

Then start a new Gemini Live call and reproduce the problem. The diagnostics module is loaded only after both the server diagnostics flag and this local opt-in are present. It is not collected for WebRTC calls. After stopping the call, inspect availability and download the artifacts from the Console:

```js
window.termLLMLivePCMArtifacts.status();
await window.termLLMLivePCMArtifacts.download();
```

Downloading during a call stops only the diagnostic capture, not the call itself. The download waits for the recorder's final data before exporting (with a bounded timeout reported in the metadata), so an immediate post-stop download includes the final audio tail.

The download produces:

- `*-provider-mono24k.wav`: the incoming signed 16-bit little-endian mono 24 kHz provider PCM, with a standard WAV header;
- `*-browser-rendered.webm`, `.ogg`, or `.mp4` when `MediaRecorder` supports recording a `MediaStreamAudioDestinationNode` and produced data; and
- `*-timings.json`: incoming and retained byte/sample counts, chunk receipt and output-worklet enqueue boundaries, interruption boundaries, leading/trailing samples, minima/maxima, rendered chunk boundaries, limits, truncation flags, and any recorder support error.

The rendered recording is a tee from the single output worklet inside the Web Audio graph. **The microphone source and input worklet are never connected to it.** Capture remains entirely in browser memory until you invoke the download function; term-llm does not upload these artifacts or put their contents in browser/server diagnostic reports. The recording describes Web Audio's rendered graph, not downstream operating-system, Bluetooth, DAC, amplifier, or speaker behavior.

Capture is bounded to 60 seconds. Incoming PCM retention is capped at 2,880,000 bytes (60 seconds of mono PCM16 at 24 kHz), browser-rendered encoded data at 8 MiB, and detailed chunk/interruption arrays at 4,096 entries. Reaching a cap is recorded in the JSON rather than growing memory without limit. Only the newest call's capture is retained; starting another opted-in call replaces the previous capture. Stopping or failing a call stops the recorder and media-destination tracks but keeps the bounded artifacts available for download. If `MediaRecorder` or `MediaStreamAudioDestinationNode` is unsupported, the call continues normally and the provider WAV plus JSON remain available.

When finished, remove the opt-in and optionally clear the retained in-memory capture:

```js
localStorage.removeItem("term-llm-live-pcm-capture");
window.termLLMLivePCMArtifacts?.clear();
```

Reloading the page also clears retained artifacts. Treat both recordings as sensitive voice data and review them before sharing.

## Web interface reliability

The browser store counts discarded stale status results, rejected stale stream callbacks, supervisor retries/recoveries, stream inactivity timeouts, review-queue validation failures, interaction reconciliations, and browser-storage failures. WebRTC diagnostics expose only aggregate active/reserved/rejected admission counts through `/v1/capabilities`; signaling credentials, attachment contents, prompts, and approval details are never included.

When diagnosing locally, correlate by truncated session/response/operation IDs. Do not export raw prompts, tokens, file contents, signaling tokens, or attachment data URLs.

### Operational thresholds

Deployment-specific monitoring should alert when any of these conditions persists:

- a stream supervisor retries beyond five attempts or for longer than two minutes;
- signaling timeout or WebRTC admission-rejection rates exceed 10% for five minutes;
- an unresolved approval or ask-user request is older than 30 minutes while its run remains active;
- browser storage migration/persistence failures recur in the same session;
- stale status or stream callback discards rise continuously rather than appearing as isolated races.

### Idempotency replay boundary

Response creation replays `Idempotency-Key` only for stateful streaming runs, using the durable session plus `client_message_id` ownership contract. Completed run events remain replayable for five minutes by default. Steering identities are retained for the live response/runtime and persisted pending-steering reload window; cancellation remains permanently scoped to its exact response ID and repeated late cancellation returns that run's current terminal state. Clients must reconcile after these windows rather than assuming an unbounded global replay cache.

## Debug logging

term-llm maintains debug logs for troubleshooting. Use the `debug-log` command to view and manage them:

```bash
term-llm debug-log                           # List recent debug sessions
term-llm debug-log list                      # List recent debug sessions
term-llm debug-log show [session]            # Show a session by number or ID
term-llm debug-log tail                      # Show current contents and follow new entries
term-llm debug-log tail --follow=false       # Show current contents and exit
term-llm debug-log search "pattern"          # Search logs for a pattern
term-llm debug-log clean                     # Clean old log files
term-llm debug-log clean --days 7            # Keep only last 7 days
term-llm debug-log export --json             # Export logs as JSON
term-llm debug-log enable                    # Enable debug logging
term-llm debug-log disable                   # Disable debug logging
term-llm debug-log status                    # Show logging status
term-llm debug-log path                      # Print log directory path
```

For `show` and `tail`, omit the session to select the most recent one, pass a list number (`1` is most recent), or use its session ID (the log filename without `.jsonl`), not a file path. `tail` reads all existing entries before following; it does not select the last N lines.

**Key flags:**
| Flag | Description |
|------|-------------|
| `--days N` | Limit to logs from last N days |
| `--tools` | Highlight tool calls and arguments (`show` only) |
| `--raw` | Show raw log entries without formatting |
| `--json` | Output as JSON |
| `--follow` | Follow new entries (`tail`, enabled by default); use `--follow=false` to exit after current contents |
