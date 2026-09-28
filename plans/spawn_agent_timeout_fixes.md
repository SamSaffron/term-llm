# Plan: keep a spawned agent alive when its caller stops waiting

Status: **design only**. No feature code has been changed. V1 preserves the *same live child execution* across waits; it does not replay or checkpoint a child that has already stopped.

## Product decision

Treat delegation as one inseparable capability with **three** model-facing tools: `spawn_agent`, `wait_agent`, and `cancel_agent`. An agent definition enables only `spawn_agent`; the registry automatically supplies the other two, with no individual opt-out. When `spawn_agent` is denied or removed by an active restriction, all three disappear. A human's host-level stop control remains available even if the model's tool permissions change.

`spawn_agent` starts a child **and initially waits** for it, returning the final answer if it finishes soon. If the initial wait expires, return a live `run_id`, not an execution error: the child keeps running **and its original `spawn_agent` UI row keeps showing its live activity**. `wait_agent` waits again for that same child; `cancel_agent` explicitly stops it if the caller gives up. The model cannot specify execution or wait deadlines through `spawn_agent`. Retain configured child `max_turns`, a bounded host-owned wall-clock execution limit, a finite default initial wait, and bounded waits in `wait_agent`.

The child must not outlive the server/session, its stores, approval route, or the permission scope under which it was admitted. No tool may return `running` unless a subsequent call can actually observe or cancel that run.

## Complete model-facing tool definitions

These are the **full v1 tool schemas and descriptions** to implement, rather than pseudocode that depends on a separate undocumented contract. JSON Schema `additionalProperties:false` applies throughout. Identifiers below are opaque strings; `session_id` is for inspection, **not** the run handle.

### `spawn_agent`

> Delegate a task to a named agent and wait briefly for its result. If the agent is still working, this tool returns `state:"running"` with a `run_id`; use `wait_agent` to give the **same agent** more time, or `cancel_agent` if its work is no longer wanted. A running child continues consuming resources until it finishes, is cancelled, reaches its configured limits, or its owner shuts down. Do not call `spawn_agent` again merely because the initial wait ended.

```json
{
  "type": "object",
  "properties": {
    "agent_name": {"type": "string", "description": "Name of an allowed subagent."},
    "prompt": {"type": "string", "description": "Self-contained task and context for the subagent."},
    "model": {"type": "string", "description": "Optional exact provider:model override; set only when the user explicitly requests it."}
  },
  "required": ["agent_name", "prompt"],
  "additionalProperties": false
}
```

**No `timeout`, `wait_timeout`, or other clock argument.** Initial wait comes from trusted host/agent configuration, not the model. Preserve existing allowed-agent, nesting-depth, model-validation, workspace and approval checks. The execution deadline is separate from the initial wait and is never extended by another tool call.

### `wait_agent`

> Wait for an existing agent run. If this wait expires, `state:"running"` means the original child is still working: wait again if its result remains useful. This tool does not restart the child or extend its execution limit. A caller cancelling this *wait* does not cancel the child; use `cancel_agent` to stop it.

```json
{
  "type": "object",
  "properties": {
    "run_id": {"type": "string", "description": "Run identifier returned by spawn_agent."},
    "wait_seconds": {"type": "integer", "minimum": 10, "maximum": 3600, "description": "Optional time to wait, in seconds; default 300. Does not change the child's lifetime."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}
```

`wait_seconds` only controls this call; using a different name from the old execution `timeout` avoids confusing the two. On completion, return the stored final result immediately, even after several waits. On a wait expiry, return a non-error `running` result; do not set `ToolOutput.TimedOut`. Repeated/concurrent waits only observe one run and must not duplicate media publication or model/tool work.

### `cancel_agent`

> Request cancellation of a previously spawned live child because its work is no longer needed. Use this when abandoning a run; do not spawn a replacement just to stop waiting. Cancellation is deliberate and terminal once cleanup settles. A completed child stays completed if completion won the race.

```json
{
  "type": "object",
  "properties": {
    "run_id": {"type": "string", "description": "Run identifier returned by spawn_agent."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}
```

Request cancellation and return a structured `cancelling` state if tools are still settling, or the actual terminal state if already settled. `wait_agent` may then observe the final `cancelled` result; do not claim cancellation has completed just because the context was signalled. Repeated cancellation is idempotent. Reject a foreign/unknown run; cancelling a completed run returns its unchanged terminal status. The host itself calls the same cancellation path on session shutdown or when it gives up ownership; the hard execution cap is enforced independently even if the model never calls `cancel_agent`.

### Shared result envelope and state machine

All three tools use the same **new-result** JSON shape, with an out-of-band `ToolOutput.Media` for ordered media artifacts. Legacy stored spawn results remain parseable even without the new fields. An invalid-arguments error raised before admission may have no `run_id`.

```json
{
  "type": "object",
  "properties": {
    "agent_name": {"type": "string"},
    "run_id": {"type": "string"},
    "session_id": {"type": "string"},
    "state": {"type": "string", "enum": ["running", "cancelling", "completed", "max_turns", "execution_timed_out", "cancelled", "failed", "run_unavailable"]},
    "output": {"type": "string"},
    "output_complete": {"type": "boolean"},
    "error": {"type": "string"},
    "type": {"type": "string"},
    "duration_ms": {"type": "integer"},
    "execution_deadline": {"type": "string", "format": "date-time"},
    "interventions": {"type": "array", "items": {"type": "string"}},
    "intervention_disposition": {"type": "string"},
    "cancelled_by_user": {"type": "boolean"}
  },
  "required": ["state", "output_complete"],
  "additionalProperties": false
}
```

Example of an initial wait expiring while the child is still executing:

```json
{"agent_name":"codebase","run_id":"opaque-live-run-id","session_id":"child-session-id","state":"running","output":"bounded partial preview","output_complete":false,"duration_ms":300000}
```

- States: `running`, `cancelling`, `completed`, `max_turns`, `execution_timed_out`, `cancelled`, `failed`, `run_unavailable`. `cancelling` is not safe to restart and remains owned until cleanup settles. Final output is immutable per child execution. A parent wait that times out does **not** produce `type:"TIMEOUT"`; an actual execution deadline does.
- Keep the existing result fields `output`, `error`, `type`, `duration_ms`, `session_id`, `interventions`, `intervention_disposition`, and `cancelled_by_user`; add only the identity/state/completeness data needed to distinguish running from final. The running `duration_ms` means elapsed execution; final `duration_ms` means total execution. Include `agent_name` on errors and update `ParseSpawnAgentResult`'s recognized-fields guard.
- A real startup failure may have a `run_id` but no `session_id`; report `failed`, never `running`. A hard execution deadline reached with no waiter is cached and returned by the next wait. Human cancellation must not be classified as an execution failure or silently retried. Completion-versus-cancel/deadline races commit **one** authoritative terminal result.
- `run_id` identifies a process-local execution but is **not** an authorization token. Every `wait_agent` and `cancel_agent` checks the active parent's session/owner identity and tool policy. On restart or loss of the in-memory supervisor, return `run_unavailable`, not `running`; persisted child transcript is inspectable but not a checkpoint. Result/media IDs are stable so repeated reads have no repeated side effects.

## Defaults, agent definitions and migration

- **Initial wait:** use the spawning agent's `spawn.initial_wait_seconds` when set, otherwise 300 seconds. Migrate existing `spawn.timeout` (e.g. built-in developer's 600) to `spawn.initial_wait_seconds` in bundled definitions. For user definitions, accept the legacy `spawn.timeout` as a deprecated *initial-wait setting* during migration, warn/document the changed semantics, and reject defining both names at once. No model-facing `timeout` argument survives.
- **Child execution cap:** 3600 seconds by default and no more than the host's configured maximum; use a clearly named, trusted host setting such as `spawn.max_runtime_seconds` if configurability is required. The child agent's existing `max_turns` and parent max-parallel/depth/allowed-agent limits still apply. A wait does not reset any limit. Parent shutdown may stop the child earlier, with a distinct terminal cause. Explicitly document the additional potential runtime/cost after the initial wait returns.
- **Legacy tool calls:** remove `timeout` from the published `spawn_agent` schema. Do not secretly accept an execution limit while claiming this is a three-tool API; old external callers passing `timeout` receive a clear validation error with guidance to use `wait_agent` and `cancel_agent`. Stored historical tool results remain parseable. This is a deliberate breaking change for callers relying on a per-call hard timeout; document it and only release it together with the replacement tools.
- **Atomic tool availability:** normalize tool lists at the registry/configuration boundary: `spawn_agent` expands to `{spawn_agent,wait_agent,cancel_agent}` in explicit lists, agent definitions, `--tools all`, and inherited child tool sets. These companions cannot be independently enabled or disabled. Reject contradictory configuration such as enabling spawn while denying one companion; do not silently override an explicit security deny. Restriction changes act on the trio as a unit, and a dynamic deny of spawn denies all three. A parent whose ability to wait is revoked can still stop a live child via authorized host controls or owner shutdown; it cannot inspect it through a disallowed model tool. Test that `wait_agent`/`cancel_agent` never grant authority to an unrelated agent just because it can guess a run ID.
- **One-shot and nested callers:** the complete trio is still registered when `spawn_agent` is allowed (no opt-out), but do not return an unusable `running` result. In v1 an ask-style one-shot run and a nested child remain synchronous up to their *configured hard execution limit* and then return a terminal result. Cache that terminal result while the owner lives: `wait_agent` returns it immediately and `cancel_agent` reports the unchanged terminal state. After owner teardown, return `run_unavailable`; do not claim there is an active child. Do not make their no-argument default silently jump from 300/600 to 3600: retain the old configured duration as the one-shot execution cap until those hosts have a durable owner.

## Existing implementation and ownership blockers

- `internal/tools/spawn_agent.go`: today `timeout` is a context deadline under the parent response, the tool blocks inside `Execute`, and its semaphore releases when the call returns. Partial output and `session_id` on timeout are not a live handle.
- `cmd/spawn_runner.go`: a fresh `cmdRunner` executes a child; `Wait()` closes admission and drains children before shared resources close. `on_complete` currently may run on partial output after an execution failure; keep this behavior, but never execute it on a *wait* expiry.
- `cmd/runner.go` and `cmd/serve_runtime.go`: the serve spawn runner, tool semaphore, approval manager and runtime are **per response**; `env.Close()` and `spawnRunner.Wait()` would block the response if an owned child were naively detached. `cmd/serve_child_runs.go` tracks live child inspection/control but does not own execution. Chat has a longer-lived runner; ask and nested child runners are one-shot. The HTTP-backed `queue_agent` job path is not a direct-spawn replacement across hosts and permissions.
- `llm.Continuation` is a process-reload handoff, not a checkpoint created by ordinary spawn timeout. The child session row does not prove a settled turn; status may be overwritten from interrupted to error by the outer runner. V1 keeps the current execution alive instead of replaying a stopped child.

## UI continuity: the original spawn call remains the progress anchor

- Keep the existing `spawn_agent` row/segment as the **one** place showing what the child is doing: agent prompt, elapsed time, provider/model and token/tool counts, nested-tool previews, diff and image activity, and the existing expand/open-child affordances. Use its original parent tool-call ID together with the owner-scoped `run_id` to correlate all subsequent child events. An initial `state:"running"` completes the *tool invocation*, not the *child run*: show the original row as actively running after the result is recorded, with an advancing elapsed timer. `wait_agent` and `cancel_agent` render their own brief tool results but never acquire, reset, or duplicate child progress. On actual child completion, cancellation, hard timeout or failure, update that **same row** once with the final state, freeze its timer and stats, close active nested-tool indicators, and retain its readable history. Do not rewrite the already-returned spawn result to pretend it was final; the row's live/terminal state is separate from that immutable tool result. Human inspection should still open the same child session.
- **Terminal/TUI:** `internal/ui/subagent_helpers.go` already attaches events, previews and diffs to the original call ID; preserve that path and transfer the progress callback's ownership to the session host after the spawn wait returns. In `cmd/ask.go` and `internal/tui/chat/update_stream_events.go`, tool-end currently calls `SubagentTracker.Remove`, which tombstones the ID; skip that on a `running` spawn result and retire it only at actual child settlement, including failure/shutdown. Teach `internal/ui/segment.go` to render a running child independently of `ToolStatus == ToolSuccess` (which represents only the returned tool call). Ensure segments already flushed to scrollback can still refresh their visible original spawn block, or hold them in an updateable view until child settlement; do not silently lose updates after a tool boundary or when a later parent turn starts. One-shot hosts remain synchronous and keep their present rendering path.
- **Serve/web:** the current `cmd/serve_subagent_progress.go` aggregator and `cmd/serve_runtime.go` callback are response-scoped and explicitly closed on response completion; they cannot carry post-handoff updates. Keep the same original call ID and snapshot shape, but promote the child progress reducer/sink to the session-owned run supervisor. Feed the initial response stream as today while attached, then publish bounded/coalesced post-response updates through an authorized session-scoped event/snapshot path, keyed by parent session, original call ID and run ID. Persist/reconcile a bounded latest progress snapshot and terminal state with that *original* spawn row so reconnect/reload, session switch, and completion without a waiter recover its correct state; do not merely issue `children.changed` (today it refreshes the child list, not the parent transcript), or append events to a closed response stream. Reuse the existing child inspector for detail, not as a replacement for progress on the spawn row. In `frontend/src/domain/response.ts`, `response.tool_exec.end` currently marks all progress terminal; distinguish the tool's `running` handoff from true child completion. In `frontend/src/domain/transcript.ts` and `frontend/src/components/Transcript.tsx`, a persisted spawn result currently appears completed and stops its timer; project child-run status and progress separately so the original row and collapsed-group summary remain live until actual settlement. Existing completed spawn rows and historical results must still render correctly. Never interpret a `wait_agent` tool end as the child's end.


1. **Schemas and capability wiring.** Update `internal/tools/spawn_agent.go` and tests for the new input/result contract; implement `wait_agent` and `cancel_agent` specs and their strict schemas in `internal/tools/`. Add names, registration, atomic trio expansion and deny validation in `internal/tools/types.go`, `internal/tools/registry.go`, agent/config selection and active tool filters; update `cmd/flags.go`. Test explicit/implicit tool lists, active restrictions, deny precedence, unauthorized/unknown IDs, and old `timeout` rejection.
2. **One owner-held live-run supervisor.** Put the run ID, hard deadline, bounded synchronized preview, cached final result/media, cancellation/done signals and concurrency slot in a session-lived owner, not a tool instance. A `spawn_agent` wait uses its own context; child execution uses an independent supervisor-owned cancellation context and deadline. On a wait expiry, return `running` without releasing the concurrency slot; release it and close `done` exactly once on actual child settlement. Keep allowed-agent/depth/model checks at spawn. Keep the synchronous execution path for one-shot/nested callers rather than pretending their per-turn runner can outlive its owner.
3. **Host lifetime, approval and reload.** In chat, reuse the session-lived runner. In serve, promote the *execution ownership* to a session/host-lived supervisor and use the existing child registry only for inspection/stop notifications. Transfer ownership before the parent response's `env.Close`; its response must finish while the child remains addressable in another turn. Route approvals through an owner/session-scoped recipient, re-check live policy, never keep one-shot grants or hang if the recipient is gone. Admit independent restart/operation ownership before reporting `running`. On owner shutdown, call the same cancellation path as `cancel_agent`, wait for tool/descendant settlement, then close stores, approval routes and providers. A plain request-context `WithoutCancel` without an owner is insufficient.
4. **Progress and finalization.** Implement the UI continuity contract above as part of the handoff, not a later polish step. Preserve the original spawn call ID in every progress/terminal notification; keep callbacks and renderable snapshots session-owned rather than closing them with the initial parent tool/response. The initial `running` result may contain a clearly labelled partial preview, not a claim of a settled transcript. Cache the final result (including intervention and media data); repeated waits are idempotent. Preserve existing `on_complete` semantics on actual child settlement and prove exactly once per child execution. Extend the existing terminal and frontend renderers/reducers, not their visual design.
5. **Deterministic regression tests.** Use channel-controlled fake providers/tools, no sleeps/network: response 1 returns `running` and closes while child remains active; response 2 waits and receives that same run's result; cancel while a tool is settling; host shutdown cancels and joins before resource close; max-turn/hard-deadline events; completion/cancel/wait races; concurrency slot retained after handoff; repeat waits/media; approvals after parent response; nesting/one-shot terminal behavior; permission-denial and foreign-parent access; no second model/tool invocation and no stale response callbacks. Add terminal UI tests for initial progress, handoff and later nested tool/diff activity **on the original spawn row** (including after scrollback flush), advancing then frozen timer, and a single terminal removal rather than tool-end tombstoning. Add serve/web reducer, persisted transcript/reconnect, and rendered component tests: progress survives closing response 1 and parent turn 2, still appears on the original spawn row and collapsed group, finishes there without a waiter, and never migrates to wait/cancel rows or replays old terminal status. Verify legacy completed spawn results.
6. **Docs and rollout.** Update tool descriptions, embedded agent configs, user docs in `docs-site/content/` and migration notes. The examples should always use `wait_agent` to wait more and `cancel_agent` when the model/user abandons a live run. Existing `queue_agent`/`wait_for_jobs` stay unchanged. Run scoped Go tests while developing, then `gofmt -w` changed Go files, `make build`, `go test ./...`, `go vet ./...`, `make complexity`; inspect the final diff. Run frontend checks only if frontend files change.

**Do not enable non-cancelling handoff on a host until it has** a reachable session-lived owner, automatic trio authorization, hard execution cap, post-handoff approval route, prompt response teardown, observable/cancellable child, tested shutdown settlement, and **continuous original-spawn-row progress through actual child settlement (including post-response/reconnect on web)**. A partially implemented host retains synchronous terminal behavior and must not return an unusable live handle or falsely completed spawn row. Exact restart of a cancelled provider stream or in-flight tool, cross-process crash recovery of the execution, and a separate `resume_agent` remain out of scope; showing a recovered terminal/unavailable state for an interrupted old row is still required.
