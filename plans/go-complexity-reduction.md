# Safe Go complexity reduction — implementation plan

Status: implemented, 2026-09-11. See `go-complexity-progress.md`,
`go-complexity-contracts.md`, and the versioned before/current reports for the
requirement-by-requirement acceptance evidence.

## Objective and scope

Reduce the difficulty of reasoning about core execution, persistence, and UI state transitions without changing observable behavior. Prioritize mixed responsibilities and implicit mutable state over raw file length. A file move is useful organization, but is not a reduction in behavioral complexity.

The initial assessment scanned the working tree, including uncommitted edits and all nested Go modules. Re-measure the implementation base before starting; line numbers and scores are not immutable targets.

| Priority hotspot | Initial complexity | Function lines | Main concern |
| --- | ---: | ---: | --- |
| `internal/llm/engine.go`: `Engine.runLoop` | 496 | 1,852 | Compaction, retry, restart, tool and provider orchestration |
| `internal/tui/chat/chat.go`: `Model.Update` | 423 | 1,615 | Stream lifecycle embedded in UI dispatch |
| `internal/tui/chat/handlers.go`: `Model.handleKeyMsg` | 277 | 1,036 | Modal precedence and many input domains |
| `cmd/ask.go`: `runAsk` | 264 | 1,204 | Invocation preparation, execution modes and finalization |
| `cmd/serve_runtime.go`: `serveRuntime.runOnce` | 256 | 989 | Lock ownership and closure-shared persistence state |
| `internal/serve/telegram.go`: `streamReplyContinuation` | 245 | 1,274 | Transport presentation mixed with session/run ownership |
| `cmd/serve_handlers_responses.go`: `handleResolvedResponses` | 176 | 622 | Admission, idempotency, runtime selection and HTTP contracts |

Production baseline: 849 files, 10,904 named functions/methods with bodies; median complexity 3; 443 functions above 20, 58 above 50, 12 above 100. Counts use 1 + `if`, loops, non-default switch/select cases, and `&&`/`||`, attributing nested function-literal decisions to their enclosing declaration. Physical lines include comments and blanks. This is cyclomatic, not cognitive, complexity; it does not measure race freedom or test adequacy.

## Non-negotiable safety rules

- No public API, CLI output, HTTP status, event ordering, database schema, persisted format, permission policy, or provider behavior changes as part of extraction.
- No new shared cross-surface execution framework. Keep engine, CLI, server and Telegram responsibilities in their current owning packages. Similar-looking code can have different durability contracts.
- Separate test additions, mechanical moves, and state/control-flow redesign into distinct commits. Do not combine a lock or transaction change with a file split.
- One responsibility per extraction. Do not replace one large function with a giant context object containing all its locals. New state objects must have a narrow purpose, explicit ownership, and a documented lifetime.
- Preserve lock acquisition order, lock duration, callback registration order, defer order, cancellation ownership, and channel close ownership during extraction. Any intended change needs a separate behavioral proposal and tests.
- Keep entry points stable and remove superseded implementations once migrated. Do not ship two production execution paths or add a refactoring feature flag.
- Work on a clean task branch/base; do not overwrite or absorb unrelated working-tree changes. Coordinate concurrent work around engine, runtime and chat ownership.

## Delivery sequence

Each row is a milestone, not a single large PR. Split it into independently verifiable changes as specified below. Complete the exit gate before moving to its dependent milestone.

| Milestone | Deliverable | Dependency | Risk |
| --- | --- | --- | --- |
| M0 | Reproducible baseline and regression contracts | None | Low |
| M1 | Explicit chat input and stream dispatch boundaries | M0 | Medium |
| M2 | Engine compaction state isolated from run orchestration | M0; use M1 as the extraction-pattern pilot | High |
| M3 | Server run persistence state and ownership made explicit | M2 stable, because server callbacks observe engine behavior | Very high |
| M4 | Ask preparation/execution/finalization separated | M0; integrate after core changes stabilize | Medium/high |
| M5 | Response admission/runtime resolution and response-run domains separated | M3 | High |
| M6 | Telegram presentation separated from run lifecycle | M2 and M3 stable | High |
| M7 | Targeted file organization and ongoing complexity ratchet | Ratchet starts at M0; file moves after owning subsystem work | Low/medium |

Recommended first tranche: M0, M1, M2 and M3. Re-measure and reassess before authorizing the rest; do not let lower-risk cosmetic splits delay the engine/runtime work. Independent test characterization can run in parallel, but do not refactor engine and its server callback consumer simultaneously.

## M0 — Establish evidence before extraction

1. Turn the scratch assessment into a reproducible, versioned analysis command in an appropriate repository tooling location, or adopt a pinned analyzer with documented equivalent counting rules. Do not depend on ignored `tmp` artifacts or install an unpinned tool in CI.
2. Include production/test separation, generated-code exclusion, all nested modules and platform-specific files. Define handling of function literals and any top-level function-valued initializers; do not silently change scope from one run to another. Test the counting rules.
3. Record complexity, function span, file size and high-complexity function count. Identify functions by package/module, relative path, receiver and name rather than line number. Account explicitly for moves/renames.
4. Establish a clean build/test/race baseline and record pre-existing failures without weakening tests or changing user config. Resolve unexplained relevant failures before proceeding.
5. For each targeted function, write a short contract inventory: inputs, state it owns, side effects, terminal outcomes, cancellation, cleanup, and ordering. Map each invariant to existing tests; add only missing coverage.
6. Capture deterministic request/event/transcript traces where useful. Normalize incidental timestamps/generated IDs, but preserve identity relationships, event order, sequence monotonicity and persisted boundaries. Do not bless new snapshots merely because a refactor changed them.

Use `internal/llm/mock_provider.go` and `internal/testutil/harness.go` where import boundaries permit; package-internal tests can use the mock directly. Use fake transports and isolated stores. Test synchronization with channels/barriers rather than timing sleeps. No live models, real API keys or network-dependent tests.

**Exit:** reproducible baseline; relevant suites green; each first-tranche contract has a concrete test or explicitly scheduled missing test. Complexity CI may initially report only; do not block the whole repository on existing debt.

## M1 — Chat: extract dispatch domains without changing precedence

### Small-change sequence

1. Add a precedence matrix around `handleKeyMsg`: selection copy versus Ctrl-C, active stream versus quit, dialogs versus composer, session transition, approval/yolo, pending steering and normal input.
2. Extract dialog-key routing first, then individual complex dialog handlers. Preserve the original order and early-return semantics. Use the smallest explicit handled/result contract that fits existing patterns.
3. Extract pending-steering and other coherent input domains. Keep global precedence visible in `handleKeyMsg`; avoid a generic handler-registration framework.
4. Characterize `Update` stale-event/session-generation filtering and terminal event cleanup before moving stream handling.
5. Extract stream events from `Update`, then separate tool lifecycle, text/reasoning/usage, compaction/model changes and terminal completion/error responsibilities as needed. A single relocated 700-line stream switch is only an intermediate checkpoint.
6. Keep common post-dispatch command composition, rendering and terminal-title scheduling in the coordinator unless tests demonstrate an equivalent narrower owner. Helpers must not bypass this accidentally.

**Test anchors:** `handlers_test.go`, `streaming_test.go`, `main_run_manager_test.go`, `session_switch_test.go`, `steering_test.go`, `steering_persistence_test.go`, dialog/selection/reload tests in `internal/tui/chat`.

**Exit:** same user-visible input precedence, stale messages rejected, completion/error/cancel restores expected composer and steering state; each extracted domain has focused tests. `Update` and `handleKeyMsg` read as ordered dispatch rather than implementations of every feature.

## M2 — Engine: isolate compaction before restructuring the main loop

### Small-change sequence

1. Characterize normal text/tool turns, provider failure before/after committed output, cancellation during provider/tool work, restart suspension/resume and model switches using scripted provider requests and emitted events.
2. Add/confirm a compaction matrix: below threshold, soft threshold, hard threshold, failed compaction, repeated overflow after compaction, fresh one-shot protection, resumed history, tool-discovery replay and resume after compaction.
3. Extract pure threshold and eligibility decisions using existing compaction utilities; do not invent duplicate policy.
4. Move only compaction-owned mutable state and its transitions into a private run-scoped controller. Specify which state it owns versus borrows, how it updates the request, and how failures/continuation decisions return to the coordinator.
5. Extract pending runtime model-switch application as a separate responsibility if still warranted. Preserve planner and native-discovery fallback behavior.
6. Re-measure and inspect the remaining loop. Propose provider-attempt or tool-turn extraction only if it has a coherent contract; do not rewrite the central retry state machine in this tranche.

**Test anchors:** `engine_test.go`, `engine_orchestration_test.go`, `compaction_test.go`, `conversation_start_compaction_test.go`, `continuation_test.go`, `steering_test.go` and relevant restart tests.

**Exit:** request history and event traces remain equivalent; no duplicate tools/committed output, no unbounded retry introduced, same suspension checkpoint and continuation behavior. Compaction state no longer relies on an interleaved cluster of run-loop locals. Run relevant existing engine benchmarks before and after; investigate meaningful changes rather than imposing an arbitrary universal percentage.

## M3 — Serve runtime: make persistence state and lifecycle ownership explicit

This is the highest-risk milestone. Do not begin with a broad rewrite of `runOnce`.

### Small-change sequence

1. Document ownership of `rt.mu`, root checkout lease, produced-message mutex, engine callbacks, collaboration reservation/fence, history and persistence cursors. Mark exactly when durable state is established and which cleanup actions run on each failure.
2. Add/confirm deterministic tests for partial append failure, initial snapshot failure, cancellation after partial output, compaction during a run, model switch, busy admission, lease loss and collaboration reserve/commit/release.
3. Extract the existing callback-shared transcript bookkeeping into a narrow private run-persistence object. Keep synchronization and callback installation order unchanged. Methods encode snapshot, initial append, incremental append, pending assistant update and compaction reconciliation responsibilities.
4. Extract history preparation and context preparation separately. Keep mutation/rollback points explicit; do not pass live history into helpers that silently acquire ownership.
5. Retain lease acquisition, runtime lock and outer cleanup in the coordinator initially. Only extract those later if the lifetime contract becomes clearer, not simply shorter.
6. Extract final accounting/persistence after intermediate failure cases are covered. Ensure cleanup cannot overwrite the first meaningful failure or revive stale history.

**Test anchors:** `cmd/serve_runtime_test.go`, `serve_compaction_test.go`, `serve_shell_collaboration_test.go`, `serve_responses_durable_test.go`, `serve_response_recovery_equivalence_test.go`, `serve_response_idempotency_test.go`, plus `internal/session` tests.

**Exit:** persisted transcript IDs/order/revisions, partial-write recovery, provider continuation and in-memory history remain equivalent; no duplicate admission/release, stale-history resurrection, race or deadlock. Run full `cmd` and session race suites, not only new helper tests.

## M4 — Ask: turn the CLI entry point into explicit phases

1. Characterize flag/config/agent precedence, stdin/default prompt, resumed session/workspace inputs, permission setup, and repeat invocation with package-level flags.
2. Extract invocation/config resolution and prepared inputs, preferring immutable results. Preserve the timing of session-dependent resolution and tool setup.
3. Separate engine/runtime preparation, then text/progressive/JSON execution paths using existing helpers. Do not introduce a generic output framework.
4. Separate final output, persistence and statistics. Keep resource ownership and error propagation visible in `runAsk`.
5. Address `askStreamModel.Update` separately if still a hotspot; do not merge CLI orchestration and UI event refactoring in one change.

**Test anchors:** `cmd/ask_test.go`, `ask_session_inputs_test.go`, `ask_resume_context_test.go`, `ask_json_test.go`, `ask_plaintext_test.go`, `ask_progressive_test.go`, `ask_output_tool_test.go`.

**Exit:** byte-level stdout/stderr contracts where stable, JSON terminal-event count/order, tool-output behavior and resume semantics unchanged; no global flag-state leakage across invocations.

## M5 — Responses: separate admission from execution, then organize lifecycle code

1. For `handleResolvedResponses`, map idempotency replay/claim, workspace resolution, first-party versus compatibility behavior, follow-up claim, model swap, runtime admission and execution. Preserve side-effect order; an early return must release only resources it actually owns.
2. Extract follow-up claiming and runtime resolution one at a time. Introduce a prepared-request result only with clearly owned cleanup responsibilities; avoid a bag of callbacks and loosely related flags.
3. Separate streaming/non-streaming execution and final response formatting once admission contracts are explicit.
4. Split `serve_response_runs.go` mechanically by lifecycle, events/subscriptions, persistence/fencing, recovery and manager responsibilities. Keep mutexes, channel ownership, method bodies and durable formats unchanged in the move commits.
5. Simplify remaining complex methods in separate changes after re-measurement.
6. Split `handleSessionByID` by route families only with a method/path matrix covering aliases, overlapping prefixes and exact error statuses. A router replacement is out of scope.

**Test anchors:** response idempotency, durable response, response-run, recovery-equivalence, project, skills and file-change suites under `cmd`.

**Exit:** idempotent replay never re-runs the provider; stale continuation rejection, model-swap cleanup, subscriber/replay behavior, event sequences and HTTP contracts preserved. Full `cmd` race tests pass.

## M6 — Telegram: presentation first, lifecycle last

1. Characterize same-chat serialization, queued follow-up/continuation, cancellation, reload, detached runtime cleanup, persistence failures and media handling using fake Telegram transport.
2. Extract Telegram text/media presentation and edit/send decisions from `streamReplyContinuation`; retain session locking, stream token registration, watchdog and cancellation in the coordinator.
3. Extract request/history construction and a narrowly scoped event accumulator separately.
4. Only then consider stream startup/finalization helpers with explicit ownership of cancellation, timers and final edits. Do not unify this with web streaming merely because both consume engine events.

**Test anchors:** `internal/serve/telegram_test.go`, `telegram_continuation_test.go`, `telegram_queue_test.go`, `telegram_reload_test.go`, `telegram_markdown_test.go`.

**Exit:** same send/edit/media sequence, continuation-message reuse and persistence ordering; no timer/goroutine leak or cross-chat interference. Run package tests and race tests.

## M7 — File organization and sustainable guardrails

### Lower-priority mechanical splits

- `internal/session/sqlite.go` (6,184 lines): schema/migrations, sessions, messages, search and push/outbox. Keep all methods on `SQLiteStore`; preserve schema text, migration registration/order and transaction boundaries.
- `internal/memory/store.go` (3,420 lines): schema, fragments, search/embeddings, images and insights. Preserve encoding formats, FTS updates and ranking/tie semantics.
- `internal/tui/chat/commands.go`: command families, after M1 settles.
- `cmd/serve_test.go` (14,403 lines): route/domain suites after implementation file moves. Preserve shared fixtures, cleanup, test names and execution assumptions; do not add `t.Parallel` as part of a move.

Run owning package tests for each split. Schema/migration work needs existing migration and transcript/FTS consistency tests even when SQL is intended to be unchanged.

Do not prioritize terminal protocol parsers or renderer rewrites solely by complexity score. Parser branches may represent genuine protocol alternatives. Any later work on nested terminal modules follows `internal/terminal/README.md`, nested verification, race/checkptr, applicable fuzzing/cross-build checks and before/after benchmarks. Root `go test ./...` does not cover those modules.

### Complexity ratchet

Start with reporting in M0; enforce after baseline and rename handling are reliable:

- New ordinary functions above 20 require explicit justification or decomposition. Above 50 requires a specific exception and contract tests, including extracted helpers; do not merely relocate a hotspot.
- Existing exceptional functions may not increase above their recorded baseline without an explicit justification and follow-up. Track exceptions with owner/subsystem, rationale and a removal milestone, not blanket per-file suppressions.
- File sizes above roughly 1,000 lines trigger an organization discussion, not an automatic failure. Avoid arbitrary 500-line splits.
- Record original function plus extracted family: maximum function complexity, count above 20/50, shared mutable state and responsibility boundaries. Summed complexity is not a strict success metric: each extracted function adds a baseline of 1.
- Do not set a hard requirement that every dispatcher must fall below 20. A documented ordered dispatcher is preferable to opaque indirection added to satisfy a metric.

## Verification and acceptance for every implementation PR

1. Characterization tests pass on the pre-extraction code. For a missing failure-path test, demonstrate that it can detect the behavior it claims to protect, preferably by a small local mutation that is reverted before committing.
2. Run narrow regression tests after each extraction, then the full owning package suite. Preserve test independence and use temporary HOME/XDG directories with the existing module cache when global configuration can interfere.
3. Format changed Go files and run the repository completion checks:

   ```sh
   gofmt -w <changed-go-files>
   make build
   go test ./...
   go vet ./...
   ```

4. Add relevant race suites. Core first-tranche changes require `go test -race ./internal/llm ./internal/tui/chat ./internal/session ./cmd`; Telegram changes also require `go test -race ./internal/serve`. Match broader CI lifecycle coverage when those owners are touched. Use Node on PATH for Go-driven JavaScript tests.
5. If nested modules change, run `VERIFY_RACE=1 scripts/verify_nested_modules.sh` and applicable terminal checks; if frontend sources change unexpectedly, split that work out or run every required frontend quality check.
6. Compare observable traces and the before/after complexity report. Inspect `git diff` (including moved-code comparison with rename detection) for accidental behavior changes and unrelated edits.
7. Record commands, results, omissions, changed ownership contracts, measured improvement and remaining hotspots in the PR. Passing tests alone is not evidence that a new state boundary is understandable.

**Acceptance is behavioral equivalence plus clearer ownership**, not a target line count. Each milestone must leave the application releasable, with independently testable responsibilities and no replacement mega-helper.

## Stop conditions and rollback

- Stop on unexplained event/transcript differences, newly timing-sensitive tests, races, lock-order changes, increased allocations/latency in a hot path, or an extraction requiring many unrelated mutable arguments.
- Reduce scope to a mechanical extraction or add missing characterization before proceeding; do not patch tests to accept changed behavior without a separate approved behavior change.
- Keep commits/PRs dependency-ordered and independently revertible. If a regression appears, revert the latest offending extraction (and dependent changes if necessary), retain useful characterization tests, and diagnose before retrying.
- No schema/config changes are planned, so rollback should be code-only. Discovering a need for data migration is a scope change, not a refactoring implementation detail.

## Progress tracking

For each milestone, track: owner, base revision, test gaps, contract inventory, PR/commit links, baseline/current metrics, verification results, unresolved risks and acceptance decision. Revisit priorities after M3 using current scores and operational evidence. This plan intentionally makes no calendar estimate until M0 exposes actual coverage gaps.
