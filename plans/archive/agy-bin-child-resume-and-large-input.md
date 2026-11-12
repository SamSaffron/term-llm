# Reliable agy-bin child continuation and large prompt transport

Status: implemented

Implemented and verified on 2026-11-12. The final design includes lossless
staged-prompt restoration for agy's additional 192,000-byte user-text limit,
discovered by probing the rebuilt CLI. Final outcomes and evidence are recorded
at the end; earlier probe limitations describe the initial investigation only.

## Scope and success criteria

Fix `agy-bin` children that replay a large transcript on follow-up and fail with
`start agy command: fork/exec .../agy: argument list too long`. This record keeps
the original investigation, implementation constraints, and final verification.
Investigation and implementation date: 2026-11-12.

Success means:

- A completed child continued with `continue_agent` restores its own native agy
  conversation and submits the follow-up, not the old review/tool transcript.
- Legitimate fresh starts and transcript fallbacks accept large text without
  putting prompt content in argv, truncating it, or asking the model to read a
  prompt file through a tool.
- Changed historical instructions still invalidate unsafe native continuation.
- Child reload, cancellation, partial failure, isolation, and cleanup remain
  correct. Diagnostics explain native resume versus transcript fallback without
  logging prompt text or credentials.

## Confirmed observations

### The subprocess transport is broken for large text

`internal/llm/providers/clibin/agy_bin.go:447` (`runCommand`) appends
`--print`, then the entire assembled prompt, to argv. There is no stdin or
prompt-file transport. The failure is before agy starts, not a model context
limit. On the probed Linux host, page size is 4096 and `ARG_MAX` is 2097152;
the separate per-string argv limit is approximately 128 KiB. Increasing the
aggregate argv limit does not solve the single-string limit.

An isolated probe against installed agy **1.5.6** reproduced `E2BIG` with one
256 KiB prompt argument. A shorter follow-up visible in the UI does not bound
the assembled prompt, which can include system instructions and replayed child
history. Successful resume also cannot protect an individually large new input.

### Native child resume already has a persistence path

`spawn_resume.go:ContinueAgent` requests the same child session with
`Resume: true`. `spawn_runner.go:prepareLifecycleChildRequest` repairs saved
history and supplies the new user instruction. Each stopped-child continuation
creates a fresh runtime/provider; retaining a Go provider object is not required.

`serve_run_history.go:prepareRunHistory` restores provider state for persisted,
stateful continuation; `serve_run_finalize.go` saves it after success and on
failure. `serve_runtime.go:restoreProviderState` / `persistProviderState` own the
store integration. Agy exports its private home, conversation ID, message count,
and prefix hash. Valid state results in `--conversation <explicit-id>` and
`grokResumeMessages` drops leading already-generated assistant/tool replay.

Do not replace this with `--continue`: the explicit child conversation ID is the
correct selector when children run concurrently.

### A transient request augmentation can falsely invalidate resume

`engine/request.go:prepareProviderRequest` calls
`engine/codemode_roster.go:withCodeModeRoster`. When code-mode cells/receipts
exist, it inserts an unpersisted developer roster immediately before the latest
user message. Agy counts and hashes the *augmented* `req.Messages` in
`agy_bin.go:Stream`, then validates the same prefix length in
`messagesForRequest` on the next request.

Example (S=system, R=runtime roster, U=user, A=assistant):

```
first provider request:  [S, R, U1]
next provider request:   [S, U1, A1, R, U2]
saved prefix:           [S, R, U1]
next prefix of length 3:[S, U1, A1]    => reset and full replay
```

A test-only Go overlay reproduces that exact provider behavior: the conversation
changes from `native-child` to empty and all five messages are returned. Changing
or removing the roster also causes false divergence. Child engine reconstruction
makes disappearance particularly relevant because code-mode receipts are not the
same thing as durable agy state.

This is a confirmed conditional bug, **not proof of the screenshot's precise
resume-loss reason**. The incident's persisted child state/debug logs were not
examined. Other legitimate fallback reasons include missing native DB/home,
old state format, actual historical edits, or an earlier unsuccessful turn that
never committed a usable provider cursor.

## Binary probe results and limits

Probes used temporary homes under `tmp/agy-investigation`, no copied credentials,
and dead loopback proxy settings. No authenticated model generation was run.

- `agy --help` advertises `--conversation`, `--continue`, and
  `--input-format stream-json` with NDJSON input on stdin.
- `agy changelog` dates streaming input to **1.1.15**, earlier than our current
  minimum **1.3.3**. No minimum-version bump is established as necessary.
- `--print` without a value is a flag error. `--print ''` with plain-text stdin
  returns `empty prompt`; agy does **not** have cursor-agent's plain stdin path.
- `--input-format stream-json --output-format stream-json` rejects a nonempty
  argv print prompt because stdin supplies the prompts. The mode can be selected
  without a print prompt argument.
- Embedded validation strings describe an `event` discriminator, `event: user`,
  a `message` object, and string or text-block `content`. These support the
  candidate envelope below; they are not a successful runtime decode test.
- Known-model streaming-input probes reached startup/authentication and timed
  out, including a 256 KiB NDJSON input. That proves startup avoids `E2BIG`, not
  that decoding, generation, or EOF handling succeeded. A nonexistent explicit
  conversation emitted a not-found warning rather than immediately failing.

Candidate input, to verify before implementation is finalized:

```json
{"event":"user","message":{"content":"the complete assembled prompt"}}
```

Do not copy Claude's `type: user` envelope: agy's validation names `event`.

## Additional real-binary finding: agy truncates model-facing user text

The first rebuilt-CLI probe successfully sent a 320,693-byte child prompt over
stdin, persisted format-2 provider state, restarted the parent CLI, and resumed
the same child/native conversation with a 315-byte follow-up. However, inspecting
the mandatory proxy's request trace revealed the first user text was only
192,025 bytes and ended with `<truncated 129114 bytes>`. Its tail instruction
never reached the model. Transport success alone was therefore not a pass.

Oversized prompts must also be staged losslessly inside the private agy home,
with a SHA-256 envelope in the stdin text. Before every model generation, the
proxy validates the anchored envelope and restores the exact original from that
private store, including historical messages on native resume. Missing, corrupt,
symlinked, mismatched, or oversized artifacts fail closed; marker-like text in
nested user prose/model/tool output is not an artifact source. Prompt storage
and existing native tool spill files use distinct trust roots. Staged prompts
persist for the private home's lifetime and share its cleanup policy.

The final real probe must put an instruction/marker at the **tail** of the large
prompt and verify both the forwarded request and the model's answer. It must
also verify restored long history on a subsequent native resume.

## Implementation sequence

### 1. Establish the streaming-input contract

Before changing production transport, exercise the exact envelope against agy
using a controlled local harness where feasible, or an explicitly authorized
minimal authenticated smoke test. Verify:

- One encoded user event, newline, and stdin EOF runs exactly one turn.
- The existing init/step_update/result event parser still applies.
- A >256 KiB input is decoded intact; also test multi-megabyte input to expose
  any NDJSON decoder/scanner limit distinct from the OS argv limit.
- `--conversation <id>` plus streaming stdin resumes the same ID on a second
  invocation and exits after the terminal turn/EOF.
- A missing conversation does not silently generate in an unrelated session.

Then use streaming stdin for **every** agy prompt, not just a threshold fallback.
Keep prompt bytes out of argv and env. Encode with `encoding/json`, not string
concatenation. Preserve `procutil` process ownership, context cancellation,
concurrent stdout/stderr draining, proxy environment enforcement, neutral cwd,
and final result/exit validation. Check for blocked stdin writes and early CLI
exit. Do not assume receipt of a result means the CLI has safely exited/flushed
its durable DB.

Keep one process per engine Stream initially; a long-lived NDJSON process is a
separate lifecycle redesign, not required to resume a durable conversation.
No automatic argv fallback, splitting into multiple model turns, silent text
truncation, or model-mediated file reads.

If supported agy versions impose an input-line limit, establish it explicitly
and either raise the supported minimum to a demonstrated working release or
return an actionable error. Do not claim unbounded input merely because argv is
small. Model context limits remain independent.

### 2. Separate durable transcript identity from turn-local context

Keep historical validation; do not simply remove the hash or ignore all
developer/system messages.

Introduce a narrow, typed internal request contract separating the stable,
provider-safe conversation messages from request-only context such as the
code-mode roster. Recommended shape: optional continuation metadata on
`core.Request` containing the stable message sequence plus explicitly identified
turn-local context. Populate it at `prepareProviderRequest`, where the engine
knows the origin of generated messages, rather than recognizing strings in the
agy provider. Preserve existing `Request.Messages` behavior for other providers.

Agy should:

1. Count/hash the stable sequence, not the augmented request.
2. On fresh start/fallback, render the complete conversation and current context.
3. On native resume, validate stable history, strip leading native-owned output,
   and render only the new logical messages plus required current turn context.
4. Preserve developer/user ordering and send current roster information even
   though it is excluded from historical identity.

Apply existing provider-safety/vision transformations consistently. No raw author,
UI, approval, or response bookkeeping should enter a semantic fingerprint just
because the struct contains it. Inspect existing metadata projection utilities
before introducing another canonicalization path. Historical system/policy text,
user text, tool results, and actual assistant content must remain protected.

Version the agy persisted boundary/hash representation when its meaning changes.
Do not reinterpret old message counts as stable counts. Explicitly identify the
one-time incompatible-state fallback; a large fallback is safe after step 1.

### 3. Make continuation selection and uncertain failures observable

Emit bounded structured diagnostics for initial start, native resume, and
transcript fallback. Include child/session association through the host's normal
context, reason, message counts, and prompt byte count. Enumerate missing state,
missing native DB/home, incompatible state version, genuine prefix divergence,
and unverified previous run. No prompt text, credentials, or raw env dumps.

Audit failure boundaries while adding child tests. Today arbitrary run errors
can leave an old successful cursor even when agy may have accepted a newer delta;
only some mismatch/proxy paths reset it. A pre-exec `E2BIG` must not be treated as
accepted input. A started-but-incomplete native turn must not blindly reuse an
old cursor and submit the same delta again. Retain a cursor only with demonstrated
commit semantics; otherwise mark/invalidate it and explain the deliberate next
continuation fallback. Do not automatically rerun tools or retry uncertain turns.

Retain the existing fail-closed proxy-generation verification and rejection of
unexpected conversation IDs before forwarding output.

### 4. Add provider and actual child-lifecycle regression coverage

Provider tests (new focused test files, avoiding growth of `agy_bin.go`, which is
already above the new-file size cap):

- Fake binary records argv and stdin; 256 KiB and multi-megabyte prompts arrive
  byte-for-byte as one decoded event and never appear in argv/env.
- Unicode, newlines, JSON escaping, NUL text, and flag-looking text survive JSON
  encoding. Invalid input/framing and early exit produce bounded errors.
- Cancellation with blocked stdin, EOF handling, stderr volume, malformed output,
  oversized output, and absent/error terminal results do not hang or leak children.
- Identical logical history with roster moved/changed/removed still resumes;
  real historical edits/truncation still reset. Metadata-only changes do not reset.
- Resumed payload contains the follow-up/current context but not the old review
  or tool-result markers. Preserve subsequent developer/user steering messages.
- State round-trip, missing DB, old format, mismatched returned ID, ephemeral
  calls, pre-exec failure, and uncertain post-start failures have explicit outcomes.

Child integration tests in `internal/app/serve` should follow
`spawn_failed_continuation_test.go` and exercise real SQLite, spawn/continue tools,
engine preparation, provider reconstruction, and a fake agy subprocess. Add only
a narrow injectable isolation seam if required; never weaken production proxy
verification to make the harness work.

Cover a successful first review followed by `continue_agent`, reconstruction of
the parent/runtime/store between runs, child script receipts on the first run,
missing-state large replay, and cancellation/partial committed tool output.
Assert child ID, persisted provider/model identity, native conversation ID,
follow-up-only payload, preserved tool evidence, and no repeated side effect.
Use the child's state, never the parent's or agy's global most-recent session.

## Verification and delivery

During implementation run focused `make test` targets for clibin, engine/core,
and serve, then `make check-changed`, `make check-changed ARGS=--all`,
`make build`, `make plans-lint`, and finally `make complexity`. Format changed Go
files. Do not regenerate ratchet baselines to hide growth. Update provider docs
if the minimum version or observable continuation semantics change.

Investigation-only checks already passed:

- Existing agy/parser, provider-state restoration, and failed-child continuation
  tests via `make test PKG='./internal/llm/providers/clibin ./internal/app/serve'
  RUN='TestAgy|TestHandleAgy|TestBuildAgy|TestServeRuntimeRestoresProviderStateFromSessionStore|TestFailedClaudeChildContinuesAfterReload'`.
- The temporary overlay probe reproduced roster-induced full replay without
  modifying production source or tracked test files.

The shipping gate is a real two-invocation agy streaming-input/resume contract
check in addition to deterministic offline regressions. The unauthenticated
startup probes alone are not sufficient to sign off on transport compatibility.

## Implemented outcome and final verification

All five requested outcomes are implemented:

1. **Large-input transport:** `agy_bin_transport.go` sends one JSON-encoded
   `event:user` message plus EOF on stdin; command-line arguments contain only
   flags. There is no large-text argv fallback or multi-turn splitting.
2. **Stable continuation:** `core.ProviderConversation` distinguishes stable
   provider-safe history from generated turn context. Engine preparation applies
   the same safety/vision projection to stable messages; agy validates its
   semantic prefix and sends only new logical messages plus current context.
   State format 2 explicitly invalidates pre-upgrade augmented-message counts.
3. **Explicit native ownership:** each child retains its own native conversation
   ID and private home through SQLite persistence and runtime reconstruction;
   agy's global `--continue` is never used.
4. **Diagnostics and failure boundaries:** bounded debug summaries show mode,
   reason, session/native ID, message counts, and prompt bytes. Pre-exec/staging
   failures preserve valid cursors; started-but-failed/unverified turns invalidate
   uncertain cursors without retrying. Subsequent explicit continuation falls
   back with preserved transcript/tool evidence.
5. **Regression coverage:** focused provider, proxy, engine/core, and real
   spawn/continue/SQLite/subprocess tests cover long input, restored native
   history, code-cell context changes, edits, old/missing state, cancellation,
   partial failure, no repeated side effects, and malformed/error output.

The actual roster-loss regression now runs three subprocesses: an initial child
creates a real MCP script receipt, live steering forces a second Stream with a
roster, then a stopped-child reload continues in a fresh engine without that
roster. All three keep the native ID. A temporary Go overlay removing typed
stable metadata makes this test fail with a new native ID; the fixed code passes.

### Additional implementation beyond the initial transport plan

For prompts over 128 KiB, `agyproxy.StagePrompt` atomically stores the exact UTF-8
original under `<private-home>/.term-llm-prompts/<sha256>.txt` and embeds it in a
hash-anchored transport envelope. Before each generation the proxy authenticates
that envelope/prefix and restores the original, even from agy's truncated native
history. `Server.SetPromptRoot(home)` is distinct from unchanged
`SetArtifactRoot(brain)`; existing tool-spill containment is not widened.
Directories are `0700`, files are `0600`, reads and expanded requests are bounded,
and missing/tampered/symlinked/mismatched artifacts fail before upstream access.
No native conversation DB changes or model-mediated file reads are required.

### Real rebuilt-binary probe

Installed agy: **1.5.6**, model `gemini-3.8-flash-high`. The parent was a
deterministic loopback OpenAI-compatible stub; the child used the real installed
agy and authenticated model generation. All sent content was synthetic. Probe
configuration, SQLite state, logs, and wire captures were isolated under `tmp/`.

- Compiled a dedicated CLI, then used its real `spawn_agent` tool to send a
  roughly 320 KB prompt whose marker and executable instruction were at the
  **tail**, beyond agy's truncation boundary.
- The child invoked the requested real `spawn_script`, persisted its receipt,
  and replied `AGY_LARGE_CHILD_OK`.
- Restarted the parent CLI process and invoked `continue_agent` on the saved
  child. The follow-up contained 321 prompt bytes, no original marker/history,
  and selected the same explicit native conversation ID. The child recalled the
  tail marker `VIOLET_649207` exactly.
- Repeated continuation using the final **`./term-llm` built by `make build`**;
  the same child/native ID and 321-byte suffix passed again.
- Initial stdin was 320,986 bytes while argv was only 158 bytes; resumed argv
  was 210 bytes. No prompt text was in argv.
- Every captured forwarded generation contained the exact full original prompt,
  including its 320,000-space padding and tail instructions. Original requests
  showed agy's real truncation; forwarded requests contained no transport marker.
  This checks restoration on the initial model call, after the script result,
  and on subsequent native resumes—not just successful subprocess startup.

Probe harness and summary: `tmp/agy-fixed-probe-final/probe_rebuilt_cli.py` and
`tmp/agy-fixed-probe-final/cli-probe-summary.json` (gitignored local evidence).

### Automated checks

Passed:

- Full `make check-changed ARGS=--all`: formatting, `make build`, root Go build,
  vet and tests, nested modules, frontend formatting/lint/typecheck/tests,
  complexity and plan lifecycle lint.
- Full changed-area check and documentation generation with `make docs`.
- Full core/engine, agyproxy, clibin and serve package tests, plus focused race
  checks for stable context, proxy restoration and child lifecycle/cancellation.
- Final `git diff --check` and `make complexity`; no ratchet baseline updates.

No production provider behavior is left pending. Deployment still requires
loading the rebuilt executable: already-running hosts retain their old code
until restarted. Existing format-1 sessions use one deliberate transcript
fallback before acquiring a format-2 native boundary. Model context windows and
the proxy's 128 MiB expanded-request bound still apply; this is not unbounded
model context. The live compatibility probe covered installed agy 1.5.6, not
every older supported release.
