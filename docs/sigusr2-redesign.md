# SIGUSR2 reload: group review and simplification proposal

Status: historical review/proposal; its whole-turn, no-cancellation recommendation
has been superseded by safe-point continuation and grace-period cancellation. See
[signals.md](signals.md) for implemented behavior, verification and explicit exclusions. This review
examined PR #1113 at `43895d049af7ce0b797ce63a23682d9f240cbcf3`.

The PR is closed. Its complete implementation and history are preserved on local
branch `archive/pr-1113-sigusr2`. The rewrite uses `rework/sigusr2-reload`, starting
from main at `58059f4e`, without modifying the other worktrees. The original
implementation was preserved rather than deleted or rewritten in place.

## Recommendation

Start a fresh implementation from main, retaining selected primitives rather than
incrementally repairing the existing continuation architecture.

**Reload should replace the executable after owned operations finish. It should
not interrupt and reconstruct an LLM run.** Treat a whole admitted operation
(including tools, persistence, descendants, and delivery) as the safe boundary,
not an individual model call or HTTP handler returning.

Proposed default: close admission, let active work finish, and exec. If a drain
deadline expires, reopen admission and report that this attempt failed. Do not
cancel tools, requeue already-started jobs, or fabricate a continuation prompt
merely to meet a reload deadline. The operator can explicitly cancel work through
its existing controls and retry. A wait-without-deadline policy is also possible;
this latency/product choice needs agreement before a rewrite.

This trades bounded reload latency and transparent mid-operation continuation
for a much smaller safety argument. Universal *immediate* safe replacement of an
arbitrary process with in-flight external effects is not achievable by a signal
handler alone. One-shot commands cannot be re-executed without potentially
repeating their original action; they must finish without replay.

## Group review

Four independent read-only reviews ran concurrently using the requested models:

- Gemini (`agy-bin:gemini-3.8-flash-high`): drain at complete-turn boundaries;
  remove the separate checkpoint stores and continuation machinery.
- Grok (`cursor-bin:grok-4.6-high`): unify transport/service coordination;
  distinguish actual reload from deferred signals and preserve real child joins.
- Opus (`claude-bin:opus-max`): rebuild from main; persistent unsupported/quarantine
  latches defeat reload in normal use; much Telegram handoff code is unwired.
- Muse (`opencode-go:muse-spark-1.3-contributor-xhigh`): central admission/drain;
  refuse on timeout rather than serialize partial execution.

Agreement: the large cost is transparent mid-operation recovery, not SIGUSR2,
process discovery, or session ownership. Disagreement: some reviewers recommend
cancellation after a grace period, and some would postpone Telegram indefinitely.
Neither is silently adopted here: the target remains all long-lived modes, with
explicit mode-specific boundary proofs.

Line-count estimates from the reviews are not implementation budgets. The diff
adds 10,042 lines across 91 files: 4,696 test lines and 5,346 other lines (including
documentation). A several-fold reduction is plausible; a universal safe reload
is not credibly a 400-line feature including all adapters and tests.

## Findings checked against the source

1. **Ordinary use can permanently disable reload.**
   `cmd/serve_exec.go:208-228` classifies routes and assigns `unsupported` for the
   rest of the boot. `:247-252` does the same for several response kinds, MCP, and
   inline provider loops. This is a lasting ban, not ownership of active work.
   New routes can silently break reload. Replace route-string eligibility with
   accounting at the actual operation/worker admission boundaries.

2. **A failed attempt can leave admission permanently closed.**
   `cmd/serve_exec.go:109-133` returns early when quarantined or when discard/tool
   settlement fails. `:520-526` sets quarantine on failed intent cleanup. The
   normal release/reopen path is skipped. Failing closed is understandable while
   a replay intent exists; eliminating that intent removes the reason for this
   particular failure state. Never weaken fencing just to reopen unsafely.

3. **The simpler HTTP coordinator is only a starting point.**
   `cmd/restart_http.go:84-126` already has a drain/exec shape, but cancels after
   grace and publishes `failed` without restoring CLI restart eligibility.
   `internal/process/process_linux.go:295-297` rejects non-ready targets. Retrying
   a raw signal and retrying through the CLI therefore differ. Separate present
   availability from the last attempt's result rather than briefly publishing
   `failed` and overwriting it with `ready` before a waiter can observe it.

4. **Telegram handoff machinery does not provide Telegram reload.**
   `cmd/serve.go:755-757` installs an unsupported owner for Telegram-only;
   `:826-829` disables replacement when Telegram accompanies HTTP. The platform
   save/take/restore functions in `internal/serve/telegram_platform_handoff.go`
   have no non-test callers. This is substantial code without a working lifecycle.

5. **TUI replacement adds model-visible recovery instructions.**
   `internal/tui/chat/process_reload.go:110-138` persists a developer message and
   restarts a response. Draft/session restoration is useful; automatic execution
   continuation is a separate, much more expensive feature. Keep them separate.

6. **Database work occurs under global admission locking.**
   `cmd/serve_exec.go:109-166` performs rollback/discard work under `c.mu`; request
   handlers call it at `:218-220` and `:227-229`. Centralization should reduce the
   state machine, not put every service's persistence under one mutex.

7. **A registry test depends on the process umask.**
   `internal/process/process_linux_test.go:75-84` requests a 0755 directory but
   does not chmod it. Under umask 077 it is actually private, so the expected
   rejection correctly does not occur. Reproduced locally. Fix the fixture when
   porting it; do not weaken registry permission checks.

### Recommendations not accepted without correction

- Do not replace start identity/pidfd checks with `kill(pid, 0)`. PID reuse is real;
  registry files are advisory, not signaling or session-write authority.
- Do not requeue already-started LLM jobs as a reload mechanism. Main's
  `requeueClaimedRunAfterShutdown` handles *unstarted claimed* work, not arbitrary
  running operations. Persisted transcripts do not prove external effects can be
  repeated safely.
- A cancelled context or returned stream is not proof of actual tool settlement.
  Drain-first still requires ownership of detached descendants and subprocesses.
- The claim that Go listeners necessarily leak across exec is not established:
  ordinary Go sockets use close-on-exec. Audit descriptors instead of closing
  listeners early and assuming reopening the admission gate restores them.
- An UPDATE affecting zero rows does not by itself establish a missing SQLite
  write lock. Do not treat that reviewer claim as a verified fencing defect.
- Delayed process publication is not by itself false restart success: the waiter
  checks a new instance and readiness. Optimize startup only after preserving
  identity and observability semantics.

## Smaller architecture

Keep the existing package separation: `internal/restart` owns the lifecycle;
`internal/process` owns advisory discovery. Do not create another competing owner.

```text
one early SIGUSR2 listener
          |
          v
one process coordinator: ready -> draining -> replacing -> new boot -> ready
                                  |
                                  +-> timeout/preparation/exec error -> ready
                                      (last attempt remains observably failed)
          |
one admission/work tracker + small mode adapters
```

### Coordinator responsibilities

- Coalesce repeated requests. An early signal for a resumable long-lived mode
  should remain pending until initialization completes, not disappear on bind.
  Install the signal disposition as early as practical; a Go handler cannot
  promise protection before the executable has initialized its signal machinery.
- Close all root admission before testing for quiescence. Allow descendants of
  admitted work; require them to register before their parent's release.
- Release an operation only after its side effects, terminal persistence, and
  owned descendants settle. Do not equate an HTTP response with a completed run.
- Keep Stop/cancel/approval controls usable while draining; reject genuinely new
  work. Read-only connections can reconnect, but GET is not proof of no effects.
- Apply one drain policy to combined modes. No separate web/jobs/Telegram reload
  state machines, no engine model-boundary callback, no tool-name allowlist.
- Prepare only minimal non-execution state (TUI composer, necessary delivery
  cursor). Prefer reversible preparation. Every resource stopped before exec
  needs explicit failed-exec restoration; gate reopen alone is insufficient.
- Centralize executable selection, argv/environment handling, and exec. Preserve
  the installed pathname rather than pinning an obsolete symlink target; test
  both atomic file replacement and symlink-based upgrades. Scrub handoff hints
  and replacement-only credentials before spawning tool children.
- TERM/INT shutdown wins before committing exec. A successful exec cannot roll
  back if the new executable later fails during startup; readiness must report
  that failure, not claim the old process was preserved.

### Identity and session ownership

Keep owner-private per-process files with PID, OS start identity, per-exec UUID,
actual build identity, mode, lifecycle state, and an observable last attempt.
Keep safe Linux pidfd targeting. Direct SIGUSR2 remains independent of discovery
and registry availability. Non-Linux targeting needs an explicit safe contract.

Use one per-exec identity consistently for session/run ownership where practical.
Keep existing SQLite response leases, transcript revisions, and fencing tokens.
A live PID file is not a session lock; a successor must obtain write authority
through the store. Cross-mode ownership should be a small store-level contract,
not another serialized LLM execution protocol. A session owner is not sufficient
by itself to represent multiple independently owned active runs.

### Mode adapters: essential, not optional hidden work

| Mode | Boundary and retained state |
| --- | --- |
| Web/API | Track mutation operations and detached response runs through terminal persistence. Passive SSE reconnects. No synthetic continuation. |
| Jobs | Pause scheduler/worker admissions before claims; finish already-started runs including completion/notification handling. Preserve queued work; never add reload-induced replay. |
| Telegram | Control polling/acknowledgment; drain all locally accepted/buffered updates and outbound sends. Preserve the processed offset and conversation association where needed. Main's buffered `GetUpdatesChan` loop is not sufficient proof of loss-free reload. |
| Hub/reverse/WebRTC | Account at the backend mutation owner as well as forwarding ingress. Connector completion alone is not backend settlement. Reconnect passive transports; explicitly own any local mutable work. |
| Widgets/shells/MCP tools | Own the real subprocess lifetime. Stop idle infrastructure reversibly; active side-effecting work must finish or cause this attempt to fail. |
| TUI/chat | Gate foreground/background submissions in the event loop; preserve session selection, composer, attachments, and necessary UI state; restore terminal before exec and recover UI on exec failure. No automatic response continuation. |
| Stateful stdio MCP/proxy | Request completion alone may not suffice: client initialization, buffered input, session/auth state can survive on inherited stdin/stdout while server memory does not. Prove a small handoff/reinitialization contract; otherwise report inability, not success. |
| One-shot ask/exec/loop/CLI | Never replay invocation argv after effects. Finish safely; report no reload. An indefinitely running invocation needs its own explicit resumable boundary rather than being mislabeled short-lived. |

## Retain / remove / rewrite

**Retain and tighten:** `internal/restart` signal/gate ideas; `internal/process`
identity/privacy/pidfd implementation and CLI; existing session fences; reverse
connector and widget join fixes; actual tool-settlement accounting.

**Remove from the replacement design:** `cmd/serve_exec.go`'s continuation owner;
`internal/session/exec_handoff.go`; `cmd/serve_jobs_restart*.go` checkpoint journal;
restart-specific `ModelBoundary` and progressive/runner resume options; Telegram
platform/conversation/steering handoff stack; TUI synthetic developer continuation.
Remove only PR-added machinery, not baseline Telegram or steering features.

**Rewrite small:** one coordinator, transport/worker admission adapters, TUI
state-only handoff, Telegram acknowledgment ownership, process status reporting.
The existing `command_handoff.go` is a possible basis for state-only transfer;
do not generalize it into an arbitrary process checkpoint framework.

Do not delete migrations from an already-used database blindly. Before removing
PR schema code, determine whether any local databases were opened by this branch
and need forward compatibility. No production database was inspected or changed.

## Implementation and proof sequence

1. Fresh branch from main; port the tracker and independently useful joins with
   their tests. Define actual-operation completion, not just cancellation.
2. One coordinator with pending/coalesced signals, reversible timeout/exec failure,
   identity/status publication, and deterministic unit tests.
3. Integrate HTTP and worker ingress plus combined-mode coordination; test no
   admission gaps, detached tools, title/background work, and subprocess ownership.
4. Integrate TUI, Telegram, and stateful transports with only necessary non-work
   state transfer. No mode is called supported until its boundary is proved.
5. Real child-process A-to-B executable replacement: same PID, new instance/build,
   ready mode, exactly one execution of each fixture side effect, repeated/early
   signals, blocked tool, failed exec and successful retry, cancellation racing
   drain, stale PID records, PTY draft preservation, poll/send boundaries, and
   combined web/jobs/reverse/WebRTC coverage.

Tests must also show that a failed attempt is observable while the original
process accepts work and can retry. Never equate signal delivery, PID survival,
old readiness metadata, or deferred logging with replacement success.

## Verification of the preserved branch

- `go test ./internal/restart ./internal/process`: restart passed; process failed
  only at the reproduced umask-dependent directory fixture above.
- `umask 022; go test -race ./internal/restart ./internal/process`: passed.
- Initial `go build ./...` required generated frontend embed assets.
- `make build`: generated the frontend assets and compiled the binary successfully.
- `go build ./...` after asset generation: passed.
- `go test -race ./cmd -run '^(TestHTTPExecOwner|TestHubReverseConnectorStopJoinsRequests|TestWebExec)' -count=1`: passed.
- Rechecked GitHub: PR closed, head matches both local branches at `43895d04`.
  Implementation diff against the archive is empty; only this proposal was added.

These checks establish buildability and focused primitive behavior, not universal
safe reload. The original PR's full-suite/CI limitations are not cleared by this
review. No live term-llm service was signaled, deployed, or reconfigured.
