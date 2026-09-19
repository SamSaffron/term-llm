# Subagent drill-down and steering in the web UI — implementation plan

Status: proposed, 2026-09-19. Not implemented. Revised after review by fable and astra; their
corrections are folded in and the staging changed materially as a result. No sign-off.

## Problem

A delegated run (`spawn_agent`) is a black box in the web UI. The parent transcript renders a
derived `subagent_progress` summary — phase, tool counts, current tool — and nothing else. The user
cannot read what the child did while it runs, cannot read it afterwards without leaving the product,
and cannot intervene when a child is visibly going the wrong way. The only recovery is to let the
child burn its deadline, read the returned blob, and re-delegate.

## Scope and contract

Four stages with independent value, in dependency order:

1. **L0** — navigate from a parent's `spawn_agent` card into the child's persisted transcript,
   read-only, with a breadcrumb back to the originating tool call.
2. **ASK** — route a child's `ask_user` prompt to the user. Requires no run identity; see below.
3. **L1a / L1b** — live child transcripts. `L1a` is turn-granular via existing store-change events.
   `L1b` is true streaming and is gated on a spike.
4. **L2** — steer a live child.

Out of scope everywhere: editing or branching a child transcript; re-running a child; surfacing
children in the sidebar; jobs-v2 migration; TUI changes; steering a depth-2 grandchild.

**Steering is scoped, and this is a product contract, not an implementation detail.** Steering is a
*local correction within the delegated assignment the parent already made*. It is not a way to change
the parent's overall goal. The UI must say so. Changing the assignment is a different feature that
requires a parent-facing instruction, not a child-result annotation.

## Verified code paths

Line numbers as of 2026-09-19. Everything below was opened and checked; claims the first draft of
this plan got wrong are marked **[corrected]**.

### Usable as-is

- `cmd/serve_children.go:130 handleSessionChildren` serves `GET /v1/sessions/{id}/children`: status,
  attention, token counters, aggregated cost, `TaskSummary`, `StartedAt`/`EndedAt`, ETag. Capped at
  100.
- `cmd/serve_children.go:92 childSpawnProvenanceForParent` maps `childSessionID → {ParentSpawnItemID,
  ParentSpawnCallID}` from the parent's last 500 messages. `ParentSpawnItemID` is the scroll anchor
  L0 needs and is confirmed unconsumed by the frontend.
- `cmd/serve_children.go:199` already calls `s.responseRuns.latestRun(child.ID)`; `:216-226` already
  reads a child `serveRuntime`'s pending prompts. The projection is written for a world where
  children own runs and runtimes. Today neither lookup hits.
- `internal/run/types.go:117-118 OnEngineReady/OnEngineDone`, invoked at `cmd/runner.go:123-127`.
  **This is the existing mid-run engine-access seam.** `internal/serve/telegram.go:2038-2050` already
  uses it for exactly this purpose.
- `internal/run/types.go:162-163 AskUserPrompter`, wired at `cmd/runner.go:397` when the run's
  `EventSink` implements it. `spawnRunSink` does not implement it — that is the entire gap for ASK.
- `cmd/serve_events.go:741` subscribes the serve event bridge to the store's change feed;
  `:694-696` translates `StoreChangeSessionTranscriptChanged` into a `session.transcript_changed`
  event. `cmd/runner.go:307` constructs the spawn runner with the **same** store the bridge watches,
  so child transcript writes very likely already emit events today. **Verify first; L1a depends on
  it entirely.**
- `frontend/src/platform/routing.ts:15 updateSessionRoute` puts the session in the URL via
  `pushState`. Back is free.
- `cmd/serve_handlers.go:1553-1558` applies `ExcludeSubagents: true` only inside the `!selectedOnly`
  branch. `selectedWebSession` (`:1585`) resolves the selector independently, so a child **does**
  resolve server-side.
- `internal/llm/engine.go:1195 QueueSteering` and `internal/session/store.go:660 PendingSteering`
  are the steering primitives.

### Blockers and corrections

- `cmd/spawn_runner.go:224` builds the child with `Platform: runpkg.PlatformConsole`. No
  `responseRun`, no `serveRuntime`, no `response.*` events under the child session id.
- **[corrected] `run.started`/`run.finished` are global.** `cmd/serve_events.go:373-377` returns
  `true` for them unconditionally — they are not session-scoped. The first draft claimed per-session
  containment. Registering child runs would fan lifecycle events to every connected client, and
  `frontend/src/stores/server-event-coordinator.ts:342-352` turns those into catalog/status
  refetches. Three children at depth 2 means bursts of global refetches. Also note
  `cmd/serve_events.go:683-687` already emits `run.started` from a store status transition to active,
  so children may already broadcast it and L1b could double-publish.
- **[corrected] `resolveAndSelectSession` prepends into the sidebar list.**
  `frontend/src/stores/selection-store.ts:210-213`: `if (!existing) this.sessionsStore.prepend(session)`.
  Navigating to a child would therefore insert it into the sidebar, violating this plan's own
  invariant that children stay invisible. L0 needs a non-prepending resolve path or a parent-id
  filter on the client list. **This also means `Transcript.tsx:633` is not permanently dead** — it
  works once the child has been resolved once by other means. It is still broken on first use.
- **[corrected] There is a second copy of the broken button** at
  `frontend/src/components/Transcript.tsx:1168-1180`, on `message.childSessionId`, for isolated-skill
  children. L0 must fix both.
- **[corrected] The spawn path never publishes `children.changed`.** The only publishers are
  `cmd/serve_branch.go:404` and `cmd/serve_skills.go:795,835`. The first draft listed this as an open
  question; the answer is no. Any feature depending on that signal for spawned children needs a
  publish added, which needs a serve hook into the runner.
- **[corrected] `webSessionEntry` has no parent field.** The first draft hedged ("confirm whether");
  the answer is no. The L0 breadcrumb needs an additive backend field, so **L0 is not
  frontend-only**.
- **[corrected] `handleSessionInterrupt` needs far more than a runtime in the map.**
  `cmd/serve_runtime.go:877-885`: when `rt.activeInterrupt` is nil the call fails with "session has
  no active stream". That state is set only by the runtime's own run loop and carries `cancel`,
  `requestCancel`, `persistPendingSteering`, `removePendingSteering`. The handler also calls
  `augmentMessagesWithMentions(…, rt, …)` and reads `rt.providerKey` (`cmd/serve_handlers.go:2162-2174`).
  "Mostly works unmodified" was wrong.
- **[corrected] The child deadline cannot be extended.** `internal/tools/spawn_agent.go:474` is
  `context.WithTimeout`, immutable once created. `:494` reads `childCtx.Deadline()` for the UI and
  `:567/:578` classify timeout by `childCtx.Err() == DeadlineExceeded`. Extension is a rewrite of a
  load-bearing part of the tool, not a timer tweak.
- **[corrected] The semaphore cannot be released mid-run as proposed.**
  `internal/tools/spawn_agent.go:462-463` holds the token under `defer` for all of `Execute`.
- **[corrected] Nested runners never receive serve wiring.** `cmd/spawn_runner.go:661` builds the
  grandchild runner via `WireSpawnAgentRunnerWithStoreAndDepth`, which knows nothing about serve. Any
  serve-injected collaborator must propagate the way `currentMediaPublisher()` does — under a mutex,
  not the unsynchronised `SetWarnFunc` shape.
- `internal/session/sqlite_sessions.go:922` — `ExcludeSubagents` is `AND s.parent_id IS NULL`.
  Children are correctly absent from the sidebar and must stay so.

## Mechanism

### L0 — read-only drill-in

**A. Repair navigation, both call sites.** Replace the `store.sessions.value.find(...)` lookups in
`Transcript.tsx:633` and `:1168` with a resolve path that does **not** prepend into the sidebar list.
Either add a `prepend: false` option to `resolveAndSelectSession` or filter client-side on the
session's parent id. Gate the button on a non-empty child id only, not on tool completion.

*Vocabulary (landed 2026-09-19, ahead of the rest of L0):* the user-facing noun is **subagent**, so
the labels are "Open subagent" for a `spawn_agent` child and "Open skill run" for the isolated-skill
child at `:1168` — which is a skill run, not a subagent, and must not be labelled as one. The vague
"Delegating work" activity text is gone: a running `spawn_agent` with no reported phase or tool now
renders no activity line at all rather than filler, and a *nested* spawn reads "Running a subagent".
Keep this vocabulary consistent through the breadcrumb, the subagent index and the composer notices.

**B. Parent field on the session entry.** Add `parent_session_id` to `webSessionEntry`
(`cmd/serve_handlers.go:1247+`). One additive field; the store has `parent_id` directly. Do not
derive it by scanning.

**C. Breadcrumb with a spawn-call anchor.** Render `{parent title} ▸ {child title}` above a child
transcript. Back calls the parent resolve **and** scrolls to the spawn card: fetch
`/v1/sessions/{parentId}/children`, match `session_id`, use `parent_spawn_item_id` with the existing
message-anchor scroll. Navigate first, scroll opportunistically; never block back on the fetch. Fall
back to plain selection when the 500-message provenance window missed.

Depth is capped at 2, so a two-level breadcrumb is sufficient. Do not build recursive walking.

**D. Read-only is a server capability, not a composer state.** A disabled composer communicates
intent but is not the contract. Once a child is an ordinary selectable session, generic session
actions become reachable through existing APIs and reused UI. Enforce server-side: **reject new
top-level runs on any session with a non-null `parent_id`**, and expose the capability to the client
so the composer can explain itself. This covers ordinary message submission and continuation of a
finished child, not just the canonical steering route. Check whether the responses admission path
already guards this; if not, add it in L0. `cmd/runner.go:108-112` strips response-run ownership for
subagents, so the two writers may otherwise share no fence.

**E. Subagent index.** A "Subagents (N)" disclosure on the parent listing the children projection,
each navigating as in A. **This is the discovery path and it is required by L1, not optional** —
with three children running, hunting for old spawn cards is not a usable switching model. It can ship
in L0 driven by polling the ETag'd endpoint; the `children.changed` signal does not exist for spawned
children yet (see corrections) and adding it is backend work that belongs with L1's hook.

**Sizing:** small-to-medium, full-stack. Not frontend-only.

### ASK — route a child's `ask_user` to the user

**This no longer depends on L1.** `cmd/runner.go:397` wires `askUserFunc` from the run's `EventSink`
when it implements `runpkg.AskUserPrompter` (`internal/run/types.go:162-163`). `spawnRunSink` simply
does not implement it. Implementing `AskUser` on `spawnRunSink` and forwarding to the parent
runtime's existing ask-user machinery — tagged with the child session id — delivers the interaction
with **no `responseRun` and no `serveRuntime`**.

A child that asks a question is requesting interaction, so answering it violates no contract. That
makes this the cheapest real intervention capability in the plan.

It is a lower-risk experiment, not a finished 80% solution. It helps children that *recognise* they
lack information; it does nothing for a child confidently doing the wrong thing. It must answer:

- What if the user never visits the child? **Attention must route to where the user actually is** —
  the parent's spawn card and the subagent index — not only into an unvisited child view. Rendering
  a question exclusively in a child nobody opens is a new way to stall silently.
- Does the wait consume the child's deadline and its semaphore slot? Today: yes to both. Decide
  explicitly and bound it.
- What happens when a second tab answers first, or the parent is cancelled with a question open?
- Does the inherited approval manager (`cmd/spawn_runner.go:656-660 SetParent`) already own a
  competing prompt path?

Treat ASK as the validation vehicle for bounded waiting, attention routing and interaction ownership
— the three things L2 needs and cannot fake.

### L1a — turn-granular live child view

If the store-change bridge already fires for child writes (`cmd/serve_events.go:694-696,741`, verified
against `cmd/runner.go:307`), a child transcript being viewed updates on `session.transcript_changed`
with no new backend concept at all. No token streaming, no live tool calls, turn-granular lag.

**Verify the bridge first.** If it does not fire, L0's "drill in while running" shows a frozen
transcript and that must be stated in the UI rather than silently misleading the user.

### L1b — true streaming (gated on a spike)

Give a delegated run a first-class run identity and event stream.

**A. Use `OnEngineReady`/`OnEngineDone`, not a new interface.** The first draft proposed a
`ChildRunObserver` whose `BeginChildRun` was called "before the child engine starts" and received
only metadata — which never yields the engine that steering later needs. `runpkg.Request` already
carries `OnEngineReady`/`OnEngineDone` (`internal/run/types.go:117-118`), invoked around the run at
`cmd/runner.go:123-127`, and `internal/serve/telegram.go:2038-2050` already uses them for mid-run
engine access. Populate those on the child execution request from a serve-injected hook. This is
strictly less new surface.

**B. Keep `Platform: PlatformConsole`.** Platform governs workspace binding and prompt templating
(`cmd/runner.go:591 localLaunch`); a delegated run is correctly a local launch. Run identity is
orthogonal and must be injected, not inferred. Flipping the platform to obtain run identity would
silently change workspace resolution for every child.

**C. The serve-side precedent is the isolated-skill child, not jobs v2.**
`cmd/serve_skills.go:770-840` already runs a serve-owned child through `runner.RunChild` with a
registered run object, a pre-allocated `ChildSessionID`, per-run event buffering, cancel, and
`children.changed` publishes at start and terminal transition. `frontend/src/stores/skill-store.ts`
already consumes `child_session_id`. Generalising *that* from skill-run to child-run is a smaller and
more consistent step than synthesising a `responseRun`. Evaluate this before writing new machinery.

**D. Tee events before down-conversion.** `cmd/spawn_runner.go:603-633 subagentEventFromLLM` drops
reasoning deltas, model switches, attempt discards and tool-argument streaming, and `spawnRunSink`
defers usage to tool boundaries (`:573-581`). A transcript rebuilt from `SubagentEvent` will not match
what a persisted reload shows — which is precisely the reconnect bug. If real fidelity is wanted, tee
`llm.Event` at the sink or pass a second sink; do not reconstruct the canonical protocol from a
summary.

**E. One execution owner.** The serve registration wraps the existing execution. It must not create a
second engine or an independent session lifecycle. Answer explicitly: who owns cancellation, provider
cleanup and terminal persistence; what happens if registration succeeds but engine startup fails; can
runtime eviction (`cmd/serve_session.go:126` janitor) dispose a running delegated engine; is a child
discoverable as active before its steering target is ready.

**F. Teardown ordering.** `cmd/spawn_runner.go:344` defers `endRun()` first, so it runs last; the
terminal status write is at `:422`. Run-finish publication must land **after** the status write and
**before** `endRun`, or `Wait()` (`cmd/serve_runtime.go:497`) returns with a run still registered, and
`cmd/serve_children.go:209-213` shows `State: active` together with `EndedAt`. The handle must also
finish on panic and on the early returns at `:342/:352/:370`.

**G. Global lifecycle fan-out is a real cost.** Per the correction above, `run.started`/`run.finished`
reach every subscriber. Browser-side subscription discipline (subscribe to a child's response channel
only while it is the active session) is necessary but **not sufficient**. Audit every consumer of
active-run registration and global lifecycle events — activity indicators, notifications, catalog
refetch — because they may equate "active run" with "top-level conversation" even though the sidebar
SQL excludes children.

**H. Reconnect is the acceptance gate, not a detail.** "Fetch messages, then attach at the current
cursor" loses any event emitted between the snapshot and cursor acquisition, and a late join may need
the start of an assistant message that is not yet durable. Required tests: joining mid-message;
disconnecting across a tool completion; replay overflow; completion racing navigation; deduplication
when streamed output becomes persisted history. A happy-path `run.started`/`run.finished` assertion is
nowhere near sufficient.

**Sizing:** large, not medium. The cost is runtime ownership, teardown and reconnect — not the
interface.

### L2 — steering a live child

**A. Steering is local correction, not goal change.** An appended note is *provenance*, not a
sufficient contract. When the parent asked for A and the user steered to B, the parent may reject B,
spawn a replacement to finish A, overwrite B while merging sibling work, or retry after cancellation
reading the outcome as failure. Sibling children continue acting on the old objective regardless.
A note at completion cannot retroactively coordinate any of that.

So: constrain the feature to corrections inside the existing assignment, say so in the UI, and record
interventions durably — identity, target run, ordered content, and **delivery disposition
distinguishing queued / consumed / finished-before-delivery**. Queue acceptance is not proof of
consumption. Carry a structured outcome into the tool result (`Interventions []string` plus
disposition on `tools.SpawnAgentResult`), including cancellation and failure, rather than a truncated
quote.

Further loss boundaries to accept or handle explicitly: truncated display text may omit the actual
requirement change; parent compaction may drop the caveat while keeping the output; if the parent is
itself delegated, the intervention may vanish when it summarises upward. Also decide whether
intervention text counts as user-authored content in the parent's context for guardian and approval
purposes (`ApprovalTranscriptPrefix`).

**B. Deadlines need a controller, not an extension.** Beyond the immutability correction above, a
child cannot outlive an ancestor's deadline by extending its own, and detaching it from ancestor
cancellation creates orphan work. Separate four things explicitly: parent cancellation and shutdown
(must always propagate); an autonomous execution budget; a bounded human-input wait allowance; an
absolute lifetime cap. Define the effective deadline across the ancestor chain. Note that extending on
*submission* does not protect a user who starts composing shortly before expiry — either accept that
race and preserve the draft, or offer an explicit bounded pause. Do not leave the depth cap as a
pressure valve.

**C. Do not exempt attended children from the semaphore. [reversed from the first draft]** Viewing a
child does not pause it: it keeps making model requests, running tools and spawning descendants while
the user reads. Tying resource admission to which session the browser has selected turns navigation
into a way to bypass the workload limit, and multiple tabs make "attended" ambiguous. A user thinking
for two minutes while holding one of three slots is an acceptable cost. If *waiting for human input*
later becomes a real execution state, give it a separate bounded waiting budget requiring slot
reacquisition before resumption — a different mechanism from "a human is looking at it".

**D. Drill-in, with persistent parent presence.** Drill-in is the right first choice: it reuses
selection, routing, history and composer, and gives deep links. Inline expansion is deferred.

But a breadcrumb answers *where am I*, not *what is still happening*. The child view must
continuously show: that the parent is waiting on this result (or its actual state); whether siblings
are running or need input; whether typing queues an instruction while execution continues; and
whether this child has finished and already returned its result.

When a child finishes while being read, **leave the reader where they are** — no auto-navigation, no
jump to bottom. Change state in place, preserve the draft, offer "Return to parent".

**E. Cancel needs a cause, not just a cancellation.** Interrupt-cancel makes `err == context.Canceled`
and the status `StatusInterrupted` (`cmd/spawn_runner.go:416`); `classifySpawnAgentError` sees the
child cancelled while the parent is not and produces a generic execution failure. `cancelled_by_user`
requires threading an explicit cause through, plus whatever partial output exists. Reuse
`llm.ClassifyInterruptImmediate` (`cmd/serve_handlers.go:2092-2104`) rather than adding a second
cancellation concept.

**F. Steering durability.** `PendingSteering` is drained by the serve run loop; `cmdRunner` has no
equivalent, and `persistPendingSteering`/`removePendingSteering` live on `activeInterrupt`. A steer
queued at the moment of child completion is silently lost today. This must be resolved, not assumed.

**G. Terminal children and conflicts.** Refuse steering a finished child in the first iteration; it
would start a fresh top-level run on an orphan session whose result nobody consumes. Do not translate
every ownership conflict into "already finished" — some conflicts mean the owner changed or the client
is stale, and the messages must differ.

**H. Approvals are an ownership change, not a rendering choice.** Children get the parent's manager
via `SetParent` (`cmd/spawn_runner.go:656-660`), so prompts surface in the parent's runtime and UI
today, and `cmd/serve_children.go:216-226` stays dead under L1b. Moving prompt ownership to a child
runtime interacts with `BindWorkspaceSessionID(parentSessionID)` (`:203-205`), which mutates the
shared parent manager on every spawn and is already questionable with three concurrent children.

**Sizing:** large. B, E, F and H are each medium alone.

## Staging and gates

| Stage | Gate before proceeding |
|---|---|
| **L0** | Child resolves server-side without entering the sidebar; both buttons fixed; persistence timing actually supports mid-run reading; read-only enforced server-side |
| **ASK** | Bounded waiting; attention routes outside the active child; answer/cancel races; no duplicate interaction ownership |
| **L1a** | Store-change bridge confirmed to fire for child writes |
| **L1b** | One execution owner; late-join/reconnect proven (H); no double accounting; safe teardown; global fan-out audited |
| **L2** | Applied-vs-queued provenance; bounded deadline policy; execution limits unchanged; explicit parent semantics; cancellation cause |

**Most likely practical failures**, in order: a late-joined child transcript that looks complete but
is missing output; and a user believing they changed the overall task when they only changed one
worker. Neither is fixed by visual polish.

## Alternatives considered

**Generalise the isolated-skill child path** (`cmd/serve_skills.go:770-840`) instead of synthesising a
`responseRun`. Probably the best option for L1b; see L1b-C. Evaluate before building.

**Jobs v2.** Rejected: couples every `spawn_agent` call to the scheduler's lifecycle and persistence
model, and changes CLI spawn behavior where no serve process exists. The feature does not need a
scheduler — but it does need scheduler-grade clarity about ownership, cancellation, waiting and
resource limits.

## Test coverage

- Extend `cmd/serve_children_test.go` for `parent_spawn_item_id` absence when provenance misses.
- L0: rejecting a top-level run on a session with `parent_id`; child resolution not entering the
  sidebar list; composer disabled with a reason; back targeting the spawn anchor.
- ASK: prompt raised from a child reaches the parent's attention surface; answer delivered to the
  correct child; second-answerer race.
- L1b: a child registers exactly one run and publishes terminal state after the status write; **with
  no serve hook (CLI path) nothing is registered and behavior is byte-identical** — the containment
  property, asserted not assumed; plus the full reconnect matrix in L1b-H.
- L2: steering a live child persists `PendingSteering` on the child; terminal child refused; stale
  `ExpectedResponseID` yields 409 distinguishably; disposition reaches `SpawnAgentResult`.
