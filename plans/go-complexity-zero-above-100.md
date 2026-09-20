# Zero functions above 100 — acceptance record

Base revision: `e99fb80c`

## Objective

Reduce every production Go function measured by `cmd/complexity` to complexity
100 or lower without changing observable behavior, ownership, ordering, durable
formats, or concurrency semantics. The checked baseline remains the ongoing
ratchet: because every recorded exception is now at most 100, `make complexity`
prevents a future function from crossing the target.

## Results

Repository summary: 917 production files, 11,126 named functions/methods,
median complexity 3, 473 above 20, 55 above 50, and **zero above 100**.

> Measured under the original cyclomatic rules. The analyzer's counting was
> revised on 2026-09-20 (see "Counting revision" in
> [`go-complexity-contracts.md`](go-complexity-contracts.md)); under the revised
> nesting-weighted measure the same unchanged code reported 601 above 20, 86
> above 50, and 10 above 100. The objective recorded here was met against the
> measure in force at the time; the remediation under the new measure is
> recorded below.

| Target | Before | After | Extracted responsibility |
| --- | ---: | ---: | --- |
| `cmd/ask.go:runAsk` | 124 | 100 | Resume/session settings and final session identity |
| `cmd/serve.go:runServeLegacy` | 147 | 100 | Startup validation/auth, project startup, tool-map parsing, shutdown, agent metadata |
| `cmd/serve_handlers_responses.go:handleResolvedResponses` | 104 | 96 | Runtime-error HTTP projection |
| `cmd/serve_runtime.go:serveRuntime.runOnce` | 123 | 93 | Persistence callback bodies and failed-run reconciliation |
| `internal/llm/engine.go:Engine.runLoop` | 441 | 40 | Per-turn orchestration, stream opening, failure/recovery and completion domains |
| `internal/llm/engine_run_turn.go:Engine.runProviderTurn` | new | 100 | Ordered one-provider-turn coordinator |
| `internal/serve/telegram.go:streamReplyContinuation` | 157 | 100 | Initial persistence and terminal outcome/finalization ownership |
| `internal/terminal/renderer/decoder.go:EventDecoder.parseCsi` | 123 | 96 | Legacy CSI key decoding |
| `internal/terminal/runtime/cursed_renderer.go:cursedRenderer.flush` | 111 | 80 | Terminal mode transitions under the existing renderer lock |
| `internal/tui/chat/chat.go:Model.Update` | 175 | 75 | Terminal, feature, and conversation dispatch families |

The engine transition sentinels represent only two pre-existing loop edges:
retry the same logical provider turn or advance to the next turn. The outer
`runLoop` retains restart/checkpoint lifecycle ownership. `runProviderTurn`
retains ordered stream/event orchestration. Compaction's request pointer is
rebound only for the lifetime of each extracted stack frame and restored before
return, preserving the single evolving request seen by compaction callbacks.

Telegram's finalizer borrows settled stream state; session locking, callback
teardown, cancellation, goroutine cleanup, and channel ownership remain in
`streamReplyContinuation`. The terminal mode helper runs synchronously while the
existing renderer mutex remains held and does not flush or invoke callbacks.

## Verification

Focused owner suites and the complete engine suite passed. Nested renderer and
runtime modules passed build, test, vet, and race verification with
`VERIFY_RACE=1 scripts/verify_nested_modules.sh`; renderer fuzzing and terminal
cross-builds also passed. Root verification completed with `make build`, an
isolated `go test ./...`, `go vet ./...`, the core and Telegram race suites, and
`make complexity`. The first core race run exposed the existing
`TestMockProvider_CancelDuringDelay` scheduling flake; ten immediate race reruns
passed, as did the subsequent owner and engine suites.

`BenchmarkProviderStreamScratchpad/Simple` remained 23 allocations and about
11.2 KiB/op. The agentic case measured 56 allocations and about 14.1 KiB/op
versus 37 allocations and 12.6 KiB/op at the base revision; measured latency was
within run variance. This tradeoff is recorded explicitly because the turn
state is now carried across independently testable ownership boundaries.

## Remediation under the revised measure (2026-09-20)

Re-measuring unchanged code with nesting-weighted counting put ten functions
above 100. Eight were reduced by mechanical extraction with no behavior change:
statement order, lock acquisition order and hold duration, defer order, HTTP
status codes, JSON field names, event/entry ordering, and rendered output were
all preserved, and each extracted block was placed in its owning file.

| Target | Before | After | Extracted responsibility |
| --- | ---: | ---: | --- |
| `cmd/serve_ask_user_state_handler.go:serveServer.handleSessionState` | 176 | 2 | Plan, rush, pending steering, runtime, persisted metadata, active-run and transcript-revision sections of one state response |
| `cmd/serve_handlers.go:serveServer.sessionMessageEntries` | 159 | 3 | Projection index, per-message entry, and per-part-kind appenders |
| `internal/ui/stream_adapter.go:StreamAdapter.ProcessStream` | 121 | 62 | Reasoning-delta, tool-call and tool-exec-end event handlers |
| `internal/ui/streaming/partial.go:StreamRenderer.findSafePoint` | 114 | 37 | Emphasis-delimiter and bracketed-link scanners |
| `internal/tui/chat/render.go:Model.viewAltScreen` | 113 | 19 | History cache, tracker changes, content build, set-content, viewport refresh and cache phases |
| `cmd/serve_response_run_recovery.go:responseRun.recoveryPayloadLocked` | 109 | 27 | Message, role-field, tool, guardian-review, attachment and event payload builders, all `Locked` |
| `internal/tooldiscovery/planner.go:Planner.selectSurface` | 106 | 65 | Authorized catalogue, run strategy, surface selection and request application |
| `cmd/serve_handlers_responses.go:serveServer.handleResponses` | 105 | 10 | Request admission, session admission, session resolution and resolved-identity admission |

`internal/terminal/renderer/terminal_renderer.go:relativeCursorMove` (120) and
`TerminalRenderer.transformLine` (116) were deliberately left alone. They are in
the owned nested renderer module, where a refactor adds recorded upstream
divergence in the hot render path; they remain baseline exceptions rather than
being changed under a metric revision.

Repository summary after the pass: 978 production files, 14,024 measured units
(function literals are now measured separately), median 3, 601 above 20, 80
above 50, and 2 above 100 — both the deliberately excluded renderer functions.

### Coverage

Every refactored function kept or improved the covered fraction of its region.
`handleResponses` was the weakest at 62.4% and is the one place where tests were
added rather than merely preserved: `cmd/serve_handlers_responses_admission_test.go`
covers malformed requests, agent/project/draft/notification admission
rejections, corrupt and conflicting previous-response mappings, and idempotency
claim release, taking the region to 85.4% (131/210 → 193/226 statements).
`internal/ui/stream_adapter_test.go` gained a case for the empty-reasoning and
tool-less tool-call returns. Region coverage elsewhere: `handleSessionState`
87.6% → 91.1%, `sessionMessageEntries` 94.7% → 95.1%, `viewAltScreen` 88.1% →
90.6%, `selectSurface` 88.6% → 89.4%, `recoveryPayloadLocked` 88.9% → 89.7%,
`findSafePoint` 100% → 100%, `ProcessStream` 65.1% → 65.9%.
