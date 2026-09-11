# Go complexity reduction progress and acceptance

Owner: package maintainers for each listed subsystem
Base revision: `4363767c`
Implementation: current worktree (2026-09-11)

## Milestones

| Milestone | Delivered ownership boundary | Initial → current target complexity / span | Test gaps / risks | Acceptance |
| --- | --- | --- | --- | --- |
| M0 | Dependency-free analyzer, counting tests, initial/current baselines, CI ratchet, contract map | repository: 849/10,931 files/functions → 906/11,089; median 3 unchanged; >20 443 → 458; >50 58 → 55; >100 12 → 9. The >20 increase is from narrow extracted functions' base scores and is tracked by the family rule. | Full/race commands recorded below | Accepted after final gates |
| M1 | Ordered key coordinator; dialog, completion, streaming/idle composer domains; stream admission plus content/tool/terminal handlers; feature handlers | `handleKeyMsg` 277/1,036 → 84/321; `Update` 423/1,615 → 175/524. Extracted stream terminal max 33. | Existing stale-event and terminal matrices retained | Accepted after chat/race gates |
| M2 | Run-scoped compaction controller and independent runtime model-switch transition | `runLoop` 496/1,852 → 441/1,581; controller methods remain below 20 | Central retry loop intentionally remains; benchmark allocations unchanged and timings within run variance | Accepted after engine/race gates |
| M3 | Run persistence ledger owns produced transcript, cursors, pending assistant, compaction reconciliation/usage; history, context and successful finalization owners | `runOnce` 256/989 → 123/453; persistence family max 25 | Outer lease/lock/cancel/callback installation deliberately retained | Accepted after cmd/session race gates |
| M4 | Prepared conversation/request, progressive and ordinary execution owners, ordered finalizer; invocation globals restored; stream model event/terminal domains separated | `runAsk` 264/1,204 → 124/714; `askStreamModel.Update` 130/548 → 78/378; execution family max 41 | Resource acquisition/defer order remains visible in coordinator | Accepted after ask/cmd gates |
| M5 | Follow-up lease, runtime plan, request projection and synchronous execution; response-run lifecycle/events/persistence/recovery/manager/stream files; event projection/recovery and run execution separated; ordered route-family dispatch | `handleResolvedResponses` 176/622 → 104/376; `handleSessionByID` 119/474 → below 20; `appendResponseRunEvent` 72/304 → below 20; `applyRecoveryEventLocked` 94/324 → 20/41; `startResponseRun` 93/393 → 55/220 | Existing HTTP/idempotency/recovery matrices retained | Accepted after full cmd race gate |
| M6 | Request-history builder, synchronized event accumulator with immutable snapshots, text/message-window presentation and media delivery | `streamReplyContinuation` 245/1,274 → 156/815; extracted presentation/accumulator methods below 20 | Lifecycle intentionally retains chat lock, watchdog, stream and persistence ownership | Accepted after serve race gate |
| M7 | Mechanical splits for SQLite, memory, chat commands, response runs and serve tests; checked baseline | target file sizes: SQLite 6,184 → 424; memory 3,420 → 688; commands 4,292 → 754; response runs 4,443 → 323; serve test 14,403 → 3,631 | Files over ~1,000 lines remain only where cross-cutting organization was not part of a safe mechanical family split | Accepted after owning suites |

Moves and renames are explicit in the current baseline identities. The original
baseline remains immutable for comparison; the current baseline is the ratchet.
Its 458 exceptions distinguish 420 original declarations, 20 mechanical moves,
and 18 newly extracted domain owners. Exceptions include owner, origin,
rationale, and removal milestone rather than per-file suppression. Hidden/cache,
`testdata`, generated, dependency, and build-output trees are excluded and
covered by analyzer fixtures.

## Benchmark record

`BenchmarkExecuteSingleToolCallFast` (3 samples): base 2,485–2,811 ns/op,
current 2,518–2,568 ns/op; both 2,703 B/op and 19 allocs/op.
`BenchmarkLoggingStreamTextDeltas`: base 114–125 µs/op, current 115–116 µs/op;
both 319 B/op and 2 allocs/op. No meaningful regression is indicated.

## Verification record

Commands completed successfully on 2026-09-11:

- `go test ./internal/llm ./internal/tui/chat ./internal/session ./internal/memory ./cmd ./internal/serve`
- `make complexity`
- `make build`
- isolated `go test ./...` with temporary HOME/XDG directories
- `go vet ./...`
- `go test -race ./internal/llm ./internal/tui/chat ./internal/session ./cmd`
- `go test -race ./internal/serve`

Nested modules and frontend sources were not modified. The analyzer reads nested
modules, while their source remained unchanged, so nested build/race and
frontend quality gates are not required by the plan's touched-area rule.
