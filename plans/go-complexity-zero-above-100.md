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

Focused owner suites and the complete engine suite pass. Nested renderer and
runtime modules pass build, test, vet, and race verification. Renderer fuzzing
and terminal cross-builds pass. Final root build, isolated full tests, vet, root
race suites, ratchet verification, report consistency, and the requested group
review are recorded after completion.
