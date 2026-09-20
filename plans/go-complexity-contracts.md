# Go complexity contracts and acceptance record

This document is the versioned M0 contract inventory for
[`go-complexity-reduction.md`](go-complexity-reduction.md). The analyzer is
`go run ./cmd/complexity`; its exact rules are emitted in every JSON report.
The initial report is `go-complexity-baseline-initial.json`, keyed by module,
path, package, receiver, and function rather than line number. The companion
`*-report.json` artifacts retain the complete file census and functions above
20; their summary counts record the complete function census, including
ordinary functions omitted from the compact checked-in view.

## Scope and counting contract

- Production and tests are separate (`--tests` opts tests in).
- All `.go` files are parsed regardless of host platform/build tags, including
  the root, `internal/reflow`, `internal/terminal/renderer`, and
  `internal/terminal/runtime` modules.
- Standard generated-file headers and repository dependency/output directories
  are excluded. `tmp`, user homes, caches, and module caches are never scanned.
- Complexity is one plus a nesting-weighted score of the body (revised
  2026-09-20; see "Counting revision" below). Top-level function-valued
  initializers are named after the initialized variable. Span and file size are
  physical lines.
- The checked baseline records every existing exception above 20 with package
  owner, rationale, and removal milestone. New functions above 20 and increases
  to existing exceptions fail `make complexity`.

## Counting revision (2026-09-20)

The original count was plain cyclomatic complexity, which measured the *shape*
of Go code rather than the cost of reading it: a flat fifteen-case dispatch
scored fifteen, a five-clause guard scored five, and every callback body was
charged to whichever function happened to pass it. Deep nesting, the thing that
actually makes code hard to hold in one's head, was free. The analyzer now
counts:

- `1` base per measured unit.
- `1 + nesting depth` for each `if`, `for`, `range`, `switch`, `select`.
  Nesting is the count of enclosing control structures within the same unit.
- `1` (flat, no nesting weight) for each `else` and each `else if`: a chain is
  one decision, not growing depth.
- `1` (flat) for a guard clause — an `if` with no `else` whose body ends in
  `return`, `break`, `continue`, `goto`, or `panic` — and its body does not
  deepen nesting. An early exit discharges a case instead of holding one open.
- `1` per *sequence* of the same logical operator, so `a || b || c` costs one
  and `a && b || c` costs two.
- `1` per labeled `break`/`continue` and per `goto`.
- Cases are free: `switch` and `select` are charged once regardless of how many
  arms they have.
- Each function literal is measured as its own unit named `<parent>.funcN`,
  numbered in order of appearance within its immediate parent unit, restarting
  at nesting zero. A literal inside a literal is therefore `f.func1.func1`, so
  its identity survives a sibling being added elsewhere in the declaration.
  Literals inherit the enclosing receiver for identity.

Rationale and consequences: the measure now ranks dense nested code above long
flat code. Straight-line dispatchers and provider `Stream` wrappers whose body
is one closure fell sharply; deeply nested parsing, rendering, and HTTP state
handlers rose. The threshold of 20 is unchanged, and the ratchet is unchanged:
the baseline was rewritten under the new measure, preserving the ownership
record of every exception that survived it. Exceptions that appear only because
the measure changed are marked `"origin": "remeasured"`; they are not new code.
Because the scale moved, complexity numbers recorded in earlier acceptance
documents are only comparable within their own measure.

## Target contracts

| Owner / target | Inputs and owned state | Side effects and terminal outcomes | Cancellation, cleanup, ordering | Characterization evidence |
| --- | --- | --- | --- | --- |
| Chat — `handleKeyMsg` | Key plus modal/composer/stream state; coordinator owns precedence only | Copy, modal updates, composer commands, send/cancel | Session transition → selection Ctrl-C → global Ctrl-C/yolo → external UI → help/attachments/steering/dialog/completion → history/cancel/navigation → streaming or idle composer | `handlers_test.go`, `session_switch_test.go`, `steering_test.go`, dialog/selection tests |
| Chat — `Update` / stream family | Bubble Tea message, run/subscription/generation IDs; stream handlers own event-domain mutations | Transcript/render/tool/usage/terminal commands | Run/subscription filtering precedes generation filtering; terminal handlers preserve flush-first and shared-tail distinctions | `streaming_test.go`, `main_run_manager_test.go`, `render_test.go`, `perf_telemetry_test.go` |
| Engine — `runLoop` / `runCompactionController` | Request and provider stream; controller alone owns run-scoped compaction latches, brief checkpoint, usage, and resume boundary | Provider/tool calls, callbacks, ordered events, request replacement | Callback before provider reset/request replacement/boundary; one reactive retry; soft failure restores tools; controller owns no stream/channel/cancel | `engine_test.go`, `engine_orchestration_test.go`, `compaction_test.go`, `conversation_start_compaction_test.go`, continuation/restart/steering tests |
| Serve — `runOnce` / `serveRunPersistence` | Runtime lock and request; persistence object owns produced transcript, append cursors, pending assistant row, reconciliation flags, compaction usage for one run | Initial/incremental/snapshot writes, response boundaries, callbacks | Root lease then runtime lock; initial durable boundary before activity commit; callback lock/order unchanged; outer coordinator owns lease, cancel and callback installation cleanup | `serve_runtime_test.go`, `serve_compaction_test.go`, `serve_shell_collaboration_test.go`, durable/recovery/idempotency suites, session tests |
| Ask — `runAsk` phases | Cobra invocation/config/session inputs; execution owners hold one mode's stream lifecycle; finalizer owns terminal output/hook ordering | stdout/stderr/JSON, session writes, tool output, hooks/stats | Store/spawn/MCP/approval defers remain coordinator-owned; interrupted streaming skips normal finalization; JSON terminal event precedes merged compaction stats | `ask_test.go`, `ask_session_inputs_test.go`, `ask_resume_context_test.go`, `ask_json_test.go`, plaintext/progressive/output-tool tests |
| Responses — admission/execution and response runs | HTTP request, idempotency claim, workspace/runtime/follow-up lease | Headers/status/body, provider run, durable event replay | Workspace before replay; runtime admission before execution; claims release unless explicitly transferred; response-run mutex/channel/fence formats unchanged by file moves | response idempotency, durable, recovery-equivalence, model-swap, selected-input admission and project suites under `cmd` |
| Telegram — `streamReplyContinuation` / presentation / accumulator | Per-chat session lock and engine stream; accumulator owns display reduction; presentation owns message window/send policy | Telegram send/edit/media and transcript persistence | Incoming persistence before admission release; text then legacy images then referenced media; lifecycle owns watchdog/ticker/cancel; final persistence still precedes returning delivery error | `telegram_test.go`, continuation/queue/reload/markdown tests and `telegram_events_test.go` |
| Session/memory organization | Existing store receivers and schemas | Identical SQL, migration order, transaction and ranking behavior | Mechanical moves only; no schema text or durable format changes | owning package migration/transcript/FTS/search tests |

## Deterministic traces

The listed suites use scripted providers, fake transports/stores, stable IDs, and
ordered event assertions. Timestamp values are asserted only where identity or
monotonic order is contractual. Existing response recovery folding,
compaction-boundary, streaming flush-order, Telegram transport, and ask JSON
sequence tests are the authoritative traces; no snapshot was re-blessed for
this refactor.
