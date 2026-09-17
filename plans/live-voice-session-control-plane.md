# Live voice session control plane

Status: **shipped, with one part superseded.** Read this note first.

Everything about the binding and the switch shipped as designed: the mutable
binding under `record.mu`, `boundSession`/`rebindLiveSession`, the shared
`switchLiveSession` behind both the tool and `POST /v1/live/sessions/{id}/session`,
`sessionDirectory` over the store, the `live.session_changed` event, and the
in-place `pushState` follow-along with symmetric UI-initiated rebinds.

**What changed afterwards is how a request reaches those tools.** This plan has the
three control tools injected into the *delegated chat turn* via
`options.runtimeSetup` + `appendResponsePassthroughTools` (see the `liveControlTools`
sections below). That was replaced twice, and the shipped design is neither: control
requests never reach a chat turn at all. Every delegation is now triaged by a fast
LLM router that owns the three tools, and hands everything else to the workspace
agent — see `plans/live-voice-control-plane-isolation.md`, Revision 3.

So: read the binding, switch, directory, event and frontend sections as the record
of what shipped; read every mention of tool exposure on a delegated turn as
superseded.

## Goal

Let the live voice call operate its own session control plane: enumerate running
sessions, list recent sessions, search transcripts, and **switch which chat
session the call is bound to** — all by voice, without the user touching the
browser or TUI.

Today a live call is permanently married to one chat session. `liveSession.sessionID`
is assigned in `newLiveSession` (`cmd/serve_live.go:96`) and then treated as
immutable by every consumer. Everything below exists to break that assumption
safely and to give the delegated execution turn read access to the session
directory.

## Current architecture (the seams we must touch)

| Concern | Location | Note |
| --- | --- | --- |
| Call state | `liveSession` struct, `cmd/serve_live.go:69-104` | `sessionID` is outside `mu` and read lock-free |
| Reverse index / one-call-per-chat invariant | `registerLiveSession`, `cmd/serve_live.go:1067-1085`; `removeLiveSession:1094` | `s.liveByChat[sessionID] = liveID`; exactly 3 consumers, all in `serve_live.go` |
| Execution routing | `serveLiveDelegator{sessionID}`, `cmd/serve_live.go:1195-1199`, used in `Steer:1203` and `Run:1259` | session ID **captured once** at controller construction (`:1053`) |
| Runtime resolution | `s.runtimeForRequest(ctx, d.sessionID)`, `cmd/serve_live.go:1278` → `cmd/serve_handlers.go:3423` | per-session runtime, provider, tools, workspace |
| Call-scoped tool authority | `tools.ContextWithLiveSettings`, `internal/tools/live_settings.go:23`; injected at `cmd/serve_response_run_execution.go:50` | pin compares binding session ID to `llm.SessionIDFromContext` |
| Tool exposure for live turns | `options.runtimeSetup`, `cmd/serve_live.go:1293-1303` | `RegisterDeferred` + `appendResponsePassthroughTools` |
| Voice-model grounding | `liveSession.executionContext`, `cmd/serve_live_context.go:18-25`; consumed in `preparePlatformContext`, `cmd/serve_run_history.go:90-122` | platform-context line rebuilt each turn |
| Browser follow-along | SSE `/v1/live/sessions/{id}/events`, names at `cmd/serve_live.go:51-58`; consumed by `frontend/src/stores/live-store.ts:258-345` | 512-event ring buffer (`:34,117-122`) |
| Running-session truth | `s.activeSessionIDs()` (`cmd/serve_handlers_status.go:341`) + durable `AttentionKindRunning` (`:89-98`) | used only by `GET /v1/sessions/status` |
| Recent list / search truth | `s.store.List` (`internal/session/sqlite_sessions.go:987`), `s.store.Search` FTS5 (`:1101`) | wrapped only by HTTP handlers |

Two facts constrain the design:

1. The voice model itself has exactly **one** tool (the provider delegation
   tool, e.g. `internal/live/openai_call.go:77-86`). New capabilities are not
   added to the realtime model — they are added to the **delegated execution
   turn**, exactly like `live_settings`. The voice model asks; the execution
   agent calls the tool and speaks the result back.
2. `live.SessionOptions.SessionID` (`internal/live/provider.go:20`) is sent to
   the provider at call creation (`x-session-id`, `internal/live/chatgpt_call.go:42-58`).
   It is a correlation label and cannot be renegotiated mid-call.

## Design

### 1. Two bugs that exist *before* any feature work

Both are consequences of the immutability assumption and both must be fixed as
part of the binding refactor.

**1a. `runtime.liveContext` is never scoped to a session (cross-session prompt leak).**
`runtimeSetup` sets `runtime.liveContext = d.live.executionContext`
(`cmd/serve_live.go:1295`) on whichever runtime the delegation resolved, and
nothing ever clears it. `executionContext` reports "live" based only on
`l.ended` (`cmd/serve_live_context.go:21-24`). After a switch A→B, every
*typed* turn on A — browser, Telegram, hub — keeps getting
"Live voice mode is active … Use live_settings" with platform mode `live`, and
the model will try `live_settings` and eat `ErrPermissionDenied`. The reset
branch in `preparePlatformContext` only fires when `rt.liveContext == nil`
(`cmd/serve_run_history.go:102-107`), so A never recovers while the call lives.

Do **not** fix this by clearing the field on rebind (that needs the runtime's
lock under `liveMu`/`record.mu` and races with runtime eviction and
recreation). Make the closure session-aware:

```go
func (l *liveSession) executionContextFor(sessionID string) func() (string, bool) {
    return func() (string, bool) {
        l.mu.Lock()
        defer l.mu.Unlock()
        if l.ended || l.sessionID != sessionID {
            return liveTextModeContext, false
        }
        return "Live voice mode is active. " + ... , true
    }
}
```

This composes correctly with the existing machinery for free: when the closure
returns `(liveTextModeContext, false)`, `preparePlatformContext:95-101` produces
`platformText + "\n\n" + liveTextModeContext` with `mode = "text"` — byte-identical
to the `liveContext == nil` reset branch at `:102-107`. So A gets the proper
"live mode is inactive" transition on its next turn with no extra code.

**1b. Tool authority must pin to the run's session, not the current binding.**
`withLiveSettingsContext` currently reads `record.sessionID`
(`cmd/serve_live_context.go:133`) and `live_settings` compares it to
`llm.SessionIDFromContext` (`internal/tools/live_settings.go:40`). The obvious
fix — read `record.boundSession()` — is **wrong**: line 50 of
`cmd/serve_response_run_execution.go` sits inside the `for {` suspend/resume
loop (`:48-71`), so it is re-evaluated after every approval or `ask_user`
pause. A delegation on A that suspends for approval while the user switches to
B would resume with binding = B, context session = A, and lose all live tool
authority mid-turn.

`executeResponseRun` already has the run's `sessionID` in scope. Pass it:

```go
runtimeRunCtx = withLiveSettingsContext(runtimeRunCtx, options.live, sessionID)
```

The correct authority statement is "this run was started by this live call for
this session" — a property of the run, stable for its whole life, immune to
switching. This also shrinks the audit sweep in §2 to `liveByChat` and the
delegator only.

### 2. Make the binding mutable and single-sourced

`cmd/serve_live.go`

```go
type liveSession struct {
    id string

    mu        sync.Mutex
    sessionID string // guarded by mu; the currently bound chat session
    ...
}

func (l *liveSession) boundSession() string // reads under mu
```

Eliminate every cached copy:

- `serveLiveDelegator` drops its `sessionID` field, keeping `server` + `live`.
- Both delegator entry points snapshot once. `Steer` is called by the controller
  **directly** (`internal/live/controller.go:378`) *and* from `Run`
  (`cmd/serve_live.go:1268`), so it is not enough to resolve "at the top of
  Run": split into `Steer(ctx, request)` (snapshots, delegates) and
  `steerSession(ctx, sessionID, request)` (does the work). `Run` snapshots once
  and calls `steerSession` with its own value. One delegation is then atomic
  with respect to a concurrent switch.
- `runtimeSetup` uses `d.live.executionContextFor(sessionID)` (§1a).

Audit sweep: `rg '\.sessionID' cmd/serve_live*.go` and confirm every remaining
read is under `mu` or a deliberate snapshot. `cmd/serve_live_test.go:1006` will
need `boundSession()`.

### 3. Atomic rebind

```go
func (s *serveServer) rebindLiveSession(record *liveSession, target string) (previous string, err error)
```

Lock discipline — **resolve and validate the target with no locks held**, then:
under `s.liveMu` (then `record.mu`, matching `startLiveController:1045-1061`):

1. refuse if `s.liveClosed` or `s.liveSessions[record.id] != record`;
2. no-op if `target` equals the current binding;
3. refuse if `s.liveByChat[target]` maps to a *different* live call still in
   `s.liveSessions` (same rule as `registerLiveSession:1077`);
4. `delete(s.liveByChat, previous)`; `s.liveByChat[target] = record.id`;
5. set `record.sessionID = target`, `record.lastActivity = time.Now()`;
6. publish `live.session_changed` via a new `appendEventLocked` — `appendEvent`
   takes `l.mu` itself (`cmd/serve_live.go:107`) and would deadlock. Publishing
   inside the same critical section also stops two racing rebinds from
   publishing out of order.

`removeLiveSession:1102` already guards with
`if s.liveByChat[record.sessionID] == liveID`, so it stays correct.
`stopLiveSession` → `removeLiveSession` deletes from `liveSessions` first, so
step 1 correctly loses a race against teardown; `closeLiveSessions` sets
`liveClosed` (`:1182`).

### 4. Centralized, transport-free session directory

The tool must not call its own HTTP API. Add `cmd/serve_session_directory.go`:

```go
type sessionDirectoryQuery struct {
    Query           string // non-empty ⇒ FTS transcript search
    RunningOnly     bool
    ProjectID       string
    IncludeArchived bool
    Limit           int
}

type sessionDirectoryEntry struct {
    ID, Title, Project, Status, Snippet string
    Number       int64
    Running      bool
    NeedsInput   bool
    LastActivity time.Time
    MessageCount int
}

func (s *serveServer) sessionDirectory(ctx context.Context, q sessionDirectoryQuery) ([]sessionDirectoryEntry, error)
func (s *serveServer) runningSessionIDs(ctx context.Context) map[string]bool
```

- `runningSessionIDs` is the genuinely shared primitive: `s.activeSessionIDs()`
  unioned with durable `listStatusAttention(..., session.AttentionKindRunning)`.
  Extract it from `handleSessionsStatus` (`cmd/serve_handlers_status.go:85-99`)
  and have the handler call it, so HTTP and voice cannot drift on "what is
  running".
- `sessionDirectory` composes `store.List{SortByActivity, ExcludeSubagents}` or
  `store.Search{...}` and decorates with `runningSessionIDs`.
- Keep this type **raw** — `time.Time`, untruncated titles — so a future HTTP or
  hub consumer can reuse it. Voice-shaped presentation (≈10 entries, relative
  times, ~200-char snippets) belongs in the tool's `Execute`.
- Deliberately **do not** refactor the full `handleSessionsStatus` /
  `handleSessionsSearch` JSON projections. Their payloads are wide and
  back-compatible for hub dashboards and cached clients
  (`cmd/serve_handlers_status.go:163-230`, `cmd/serve_handlers.go:1650-1676`).
  Share the *truth*, not the wire shape.

**No fuzzy selector resolver.** An earlier draft proposed a tolerant
`resolveSessionSelector` (number → ID → prefix → unique title match, with
structured ambiguity errors). Cut it: that is a *model* problem, not a Go
problem. The agent calls `session_directory{query:"reflow"}`, reads the result,
and calls `live_switch_session{session: 42}`. Ambiguity handling is then free —
it is just the list output, and the agent can ask a clarifying question in
speech. The switch tool accepts only a durable number or an exact/prefix ID
(`store.GetByNumber` / `Get` / `GetByPrefix`, `internal/session/store.go:49-51`).
That removes ~150 lines of code and tests.

### 5. Two new tools

Follow the `live_settings` precedent (`internal/tools/live_settings.go`): a
context binding validated against `llm.SessionIDFromContext`, so a registered
schema alone grants nothing and subagents never inherit authority.

- **`session_directory`** (read-only). Args `{ query?, running_only?, project?, limit? }`.
  Deliberately *not* named `live_*`: it is a general capability, not a live-call
  control. Give it its own `ContextWithSessionDirectory` binding.
- **`live_switch_session`** (mutating). Args `{ session }` — durable number or
  ID/prefix. Returns the new binding plus the target's title, project, running
  state, and last activity. Keeps the live binding.

The split is *not* justified by `internal/tools/permissions.go` — that file is
filesystem and shell path allowlisting only (`IsPathAllowedForRead`,
`AddShellPattern`) and has no read/mutate tool taxonomy. The real reasons are
(a) the directory is reusable beyond live and (b) a rebind is a distinct side
effect worth naming in transcripts.

Even so, `session_directory` is injected **only for live turns in v1**. Exposing
every transcript to ordinary turns, subagents, and Telegram-origin sessions is a
policy change that deserves its own review.

`runtimeSetup` (`cmd/serve_live.go:1293-1303`) now wires three tools, so extract:

```go
func liveControlTools(runtime *serveRuntime, record *liveSession, sessionID string) []llm.ToolSpec
```

Because `runtimeSetup` runs per delegation against
`runtimeForRequest(ctx, sessionID)`, the control tools land on the new session's
runtime automatically after a switch.

### 6. Switch semantics

- **Effective next turn.** The switch executes inside a delegation already
  running on A; A keeps that turn, B gets the next one.
- **A's turn becomes voice-orphaned.** Once bound to B, the user can no longer
  steer A's still-streaming turn by voice: the next `Run` resolves B, sees no
  active run there, and starts a fresh turn while A is still going. Accept this,
  but say it in the tool description — "effective next turn" alone reads as if A
  is still reachable. (Alternative, rejected as over-engineering: defer the
  rebind to the switching run's completion hook.)
- **Refuse** when: target missing; target `IsSubagent`; target already hosts
  another live call; call ended. **Allow** a target with an active run — but the
  result must warn that *the next spoken request will be queued as guidance to
  that task rather than answered*, because `Run` routes it through `Steer`
  (`cmd/serve_live.go:1267-1277`). Without that sentence the user asks "what's
  it doing?" and gets silence plus a confusing steering injection. Include the
  running task's last user message in the switch result so the agent can
  describe it.
- **Archived** targets: allow, and report it.
- **Provider label staleness.** `x-session-id` keeps pointing at A. Accept and
  comment; it is correlation only.
- **Host note is mandatory, not optional.** `liveSessionOptions` already told the
  voice model `session_id: A` in its facts blob
  (`cmd/serve_live_context.go:89-93`). Without a short `AppendText` note naming
  the new binding, the model keeps saying A out loud. Do **not** re-snapshot B's
  history into the voice conversation — that rewrites the spoken transcript and
  burns tokens.
- **Idle watchdog.** `watchLiveIdle` keys off `lastActivity`; rebind touches it.
- **Browser follow-along.** Add `live.session_changed` to the constants
  (`cmd/serve_live.go:51-58`). The 512-event ring buffer (`:34,117-122`) can
  evict it, so a reconnecting tab or hub viewer may never see it: also carry
  `session_id` in `live.started` (`:1020`) and in every `live.delegation` event
  (`:1026`), which makes the UI self-healing for free.

### 7. Non-goals for v1

- Cross-node switching. The hub aggregates per-node `/v1/sessions/status`
  (`cmd/serve_hub_nodes.go:193-244`); switching needs reverse-connection RPC and
  a renegotiated call. Restrict selectors to the local store and refuse clearly.
- Creating a new session by voice (the frontend has `createBlankSession`).
- `session_directory` for non-live turns.
- Multiple concurrent live calls per chat session — still a non-goal
  (`plans/live-voice-mode.md:26-28`).

### 8. Verified non-issues

Recorded so reviewers do not re-litigate them:

- Audio tokens and PCM diagnostics are keyed by `live_id`
  (`cmd/serve_live.go:501,515,1007`), not session — no scoping change.
- `SetCurrent` fires when B's runtime loads on the first delegation
  (`cmd/serve_runtime.go:1171-1178`), not at switch time. Acceptable; note it.
- `liveByChat` has exactly three consumers, all in `serve_live.go`.

## UX: the switchover

Target feel: the live panel and its voice transcript **never move and never
reset**. The transcript above them cross-fades to the new session, the URL
updates via `pushState`, and Back works. No reload, no remount, no spinner
takeover, no dropped call.

Most of that machinery already exists and is reusable as-is.

### What already works in our favour

- `SelectionStore.selectSession` (`frontend/src/stores/selection-store.ts:64-142`)
  is already a complete in-place swap: it swaps transcript, composer draft,
  plans, goals, branches, review diff and skills through signals, persists
  `keys.activeSession`, and calls
  `updateSessionRoute(prefix, session, replace)` (`:99`) which is exactly a
  `history.pushState` (`frontend/src/platform/routing.ts:15-22`). **Nothing
  remounts.** This is the seamless navigation we want; we do not need to build it.
- `LiveStatus` renders inside `Composer` (`frontend/src/components/Composer.tsx:609`),
  which is not keyed by session, and `LiveStore` is app-scoped
  (`store.liveStore`). The panel and its `recentTurns` / `partialAssistant`
  signals therefore survive a `selectSession` **by construction**.
- A session missing from the sidebar list already has a resolve path:
  `resolveAndSelectSession(slug, true)` (`frontend/src/stores/app-store.ts:673`).

### Four things that actively break it

**UX-1 (blocker): switching sessions hangs up the call.**
`AppStore.selectSession` (`frontend/src/stores/app-store.ts:958-962`):

```js
if (session.id !== this.activeSessionId.peek()) {
  this.shellStore.back();
  void this.liveStore.stop();   // ← hangs up the phone
}
```

Any follow-along built on `selectSession` stops the very call that requested the
switch. Add an options argument:

```js
async selectSession(session, replace = false, options: { keepLive?: boolean } = {})
```

`keepLive` skips only the `liveStore.stop()`. Keep `shellStore.back()` — the
shell overlay belongs to the old session and should close.

**UX-2 (blocker): the transcript panel resets itself on session change.**
`LiveStatus` keys its scroll/expand reset on
`` const owner = `${live.sessionId.value}:${live.liveId.value}` `` and collapses the
expanded transcript when `owner` changes
(`frontend/src/components/LiveStatus.tsx:20-26`). That reset is correct for a
*new call*; for a session switch *within* one continuous call it collapses the
transcript mid-sentence — precisely the jarring behaviour we are trying to
avoid. Key on `live.liveId.value` alone: the call is the transcript's identity,
not the session.

**UX-3: the store's `sessionId` gets reverted.**
`applyCallSnapshot` overwrites `this.sessionId.value = snapshot.sessionId` on
**every** snapshot (`frontend/src/stores/live-store.ts:258-262`), and the
`LiveCall` handle captured the original session at `start(sessionId)` (`:215-218`).
Setting `sessionId` from the event gets silently reverted by the next phase
change. Stop mirroring `sessionId` from the call snapshot — after start, the
store is the source of truth.

**UX-4: the event must carry enough to navigate without a fetch.**
`updateSessionRoute` prefers the durable number (`routing.ts:4-6`), so
`live.session_changed` carries `{session_id, session_number, title}` (§6). The
client looks up the session in `this.sessions`; on a miss it falls back to
`resolveAndSelectSession`. Navigation happens immediately either way.

### Voice-initiated switch, end to end

1. Tool calls `rebindLiveSession`; server emits `live.session_changed`.
2. `LiveStore` receives it on the existing SSE stream and hands off to the app
   store (the store owns live state; **navigation is not its job** — add a
   `onSessionChanged` callback supplied by `AppStore`, mirroring how
   `LiveStore` already receives `() => 'session'` for session resolution).
3. `AppStore` resolves the session and calls
   `selectSession(session, false, { keepLive: true, fromLive: true })` —
   `replace = false`, so it is a real `pushState` entry and **Back returns to the
   previous session**; `fromLive` stops the follow-along from POSTing the switch
   back to the server.
4. `LiveStatus` never unmounts; only the transcript region above it changes.
5. Scroll the new transcript to tail via the existing
   `requestTranscriptScrollToTail()` (`Composer.tsx:416`).

### Making the change legible

A transcript silently swapping under the user is disorienting even when it is
technically seamless. Two cheap affordances, both inside the existing panel:

- A binding line in `LiveStatus`' header: `Working in #42 · Fix reflow crash`,
  updated from the event. This is the only persistent indication of *which*
  session the voice drives, and it earns its place independently of switching.
- Announce the transition in the existing `role="status"` `aria-live="polite"`
  copy (`LiveStatus.tsx:45-52`) so screen readers are told the context moved.

CSS: a short cross-fade on the transcript container only. The live panel must
not animate — it is the fixed point the eye holds onto.

### Symmetric switching: the UI moves the call too (accepted)

Today, clicking another session while a call is live hangs it up (UX-1). Voice
moving the UI without the UI being able to move voice is an asymmetry users hit
within minutes, so both directions ship together. The server stays the single
authority for the binding, and `live.session_changed` stays the single event —
the only new thing is a second way to ask for a rebind.

**Endpoint.** Add one action to the existing dispatch in `handleLiveSessionByID`
(`cmd/serve_live.go:535-558`), modelled directly on `handleLiveSessionText`
(`:561-575`) — method check, `lookupLiveSession`, `MaxBytesReader`-bounded decode:

```
POST /v1/live/sessions/{live_id}/session   {"session_id": "..."}
```

It performs the same validation as the tool (missing / subagent / already bound
to another call / call ended) and calls the same `rebindLiveSession`. The tool
and the endpoint are two callers of one mutation; neither gets its own rules.

- `200` with the new binding on success, including a `no_op: true` when the call
  was already bound there.
- `409` when the target already hosts another live call.
- `404` for an unknown `live_id`, matching the sibling actions.

**Client call site.** `AppStore.selectSession` calls it when
`liveStore.active.peek()` is true, *instead of* today's `liveStore.stop()`.
Navigation stays optimistic — the UI moves immediately and does not wait on the
round trip, because the local transcript swap is already correct regardless of
the binding outcome.

**Loop prevention (the detail that will bite).** The follow-along path must not
re-POST the switch it is reacting to. Distinguish the origin explicitly:

```js
selectSession(session, replace, { keepLive: true, fromLive: true })
```

`fromLive` suppresses the outbound rebind request. Without it the sequence is
event → `selectSession` → POST → server no-ops (already bound, §3 step 2) → no
event, which terminates only because the rebind is idempotent. Relying on that
is fragile; make it explicit.

**Echo is naturally harmless.** When the client initiates, the resulting
`live.session_changed` arrives for a session the UI already shows, and
`selectSession` short-circuits on `session.id !== activeSessionId`
(`app-store.ts:959`). No special casing needed.

**Race: user clicks A→B while the voice tool switches A→C.** The server
serializes both through `rebindLiveSession` under `liveMu`, so exactly one wins
and it emits exactly one event. The client must not let a stale in-flight
response override a newer intent: guard the POST with the selection generation
(`SelectionStore.generation`, `selection-store.ts:60-62`) and ignore a response
whose generation is stale. The authoritative `live.session_changed` then settles
the UI.

**Failure handling.** On `409` or transport failure, toast and fall back to
today's behaviour of stopping the call — the user asked to be somewhere else,
and a call still bound to a session they have left is the desync we are
eliminating.

**Back needs no extra code.** The `popstate` handler already routes through
`selectSession` (`app-store.ts:664-674`), so Back after a voice-initiated switch
moves the call back as well — which is what Back visibly implies — without a
separate code path.

## Test plan

- `internal/tools/`: denial with no binding, nil callbacks, empty session ID,
  mismatched `llm.SessionIDFromContext`; arg validation. Mirror
  `internal/tools/live_settings_test.go`.
- `cmd/serve_live_*_test.go`:
  - rebind swaps `liveByChat`, rejects a target already hosting a live call, and
    loses cleanly to concurrent `stopLiveSession`;
  - a delegation after rebind resolves the *new* runtime (mock provider via
    `internal/llm/mock_provider.go`, harness via `internal/testutil/harness.go`);
  - an in-flight delegation keeps its original binding across a concurrent
    rebind, including across a suspend/resume cycle (regression for §1b);
  - **regression for §1a:** rebind A→B, then run a typed turn on A and assert the
    injected platform message is `liveTextModeContext` with mode `text`;
  - `live.session_changed` ordering and suppression after `ended`;
  - `POST /v1/live/sessions/{id}/session`: success, `no_op` when already bound,
    `409` when the target hosts another call, `404` for an unknown `live_id`, and
    rejection of subagent/missing targets — asserting it shares the tool's
    validation rather than re-implementing it;
  - control tools stay absent from ordinary turns — extend
    `cmd/serve_live_context_test.go:121-129`.
- Directory: `sessionDirectory` running/recent/search cases against a temp
  SQLite store; assert `GET /v1/sessions/status` output is unchanged after the
  `runningSessionIDs` extraction.
- Frontend:
  - `live-store.test.ts`: `live.session_changed` updates the binding and is **not**
    reverted by a subsequent call snapshot (UX-3);
  - `app-store.test.ts`: a live follow-along switch calls `pushState` with the new
    slug and does **not** call `liveStore.stop()` (UX-1); a manual switch without
    a live call keeps today's behaviour; a manual switch **with** a live call
    POSTs the rebind instead of stopping; a follow-along switch does **not** POST
    (`fromLive` loop suppression); a refused rebind toasts and stops the call;
  - `LiveStatus.test.tsx`: an expanded transcript stays expanded and keeps scroll
    position across a `sessionId` change, and collapses across a `liveId` change
    (UX-2);
  - then `npm --prefix frontend run format && lint && typecheck && test`.
- `-race` across `cmd` for the binding change.
- Docs: `docs/live-voice.md` (tools, switch semantics, event list).

## Delivery: one changeset

This lands as a single coherent change. Shipping the switch tool and its UI
handling together is a requirement anyway (§6: UI desync is the worst failure
mode), and the two pre-existing bugs in §1 are only reachable once switching
exists, so splitting them out would mean landing dead code.

Internal build order — keep each step compiling and testable so the work
bisects cleanly even inside one commit, and so a regression can be attributed:

1. **`runningSessionIDs` extraction** from `handleSessionsStatus:85-99`, with the
   `GET /v1/sessions/status` unchanged-output test. Pure refactor; prove it before
   anything depends on it.
2. **`sessionDirectory`** + its store-level tests (running / recent / FTS search).
3. **`session_directory` tool** + binding + denial tests, wired into
   `runtimeSetup`. At this point voice can list and search but not switch.
4. **§1a `executionContextFor(sessionID)`** + the abandoned-session platform-context
   test. Correct on its own merits before the binding can move.
5. **§1b pin-to-run**: thread `sessionID` into `withLiveSettingsContext` at
   `cmd/serve_response_run_execution.go:50`, plus the suspend/resume authority test.
6. **Mutable binding**: `sessionID` under `mu`, `boundSession()`, delegator
   de-caching, `Steer`/`steerSession` split. Run `-race` here.
7. **`rebindLiveSession`** + `appendEventLocked` + `live.session_changed` constant,
   with the concurrency tests (target already bound, race with `stopLiveSession`).
8. **`live_switch_session`** tool: validation (missing / subagent / already-bound /
   ended), running-target warning text, mandatory `AppendText` host note.
9. **Event `session_id` enrichment** on `live.started` and `live.delegation`.
10. **Frontend, in UX dependency order**: UX-3 snapshot ownership → UX-2
    `LiveStatus` keyed on `liveId` → UX-1 `selectSession({keepLive})` →
    `live.session_changed` handling and `onSessionChanged` wiring → binding line
    and aria announcement → transcript cross-fade CSS.
11. **Symmetric manual switch**: `POST /v1/live/sessions/{id}/session` handler
    reusing `rebindLiveSession`, then the `AppStore.selectSession` call site with
    `fromLive` loop suppression and the generation guard.
12. **Prompts and docs**: `internal/live/prompts.go`, `executionContext` wording,
    `docs/live-voice.md`.

### File inventory

| File | Change |
| --- | --- |
| `cmd/serve_live.go` | `sessionID` under `mu`, `boundSession`, `appendEventLocked`, `rebindLiveSession`, `live.session_changed` constant, delegator de-caching, `Steer`/`steerSession`, `liveControlTools`, event `session_id` |
| `cmd/serve_live_context.go` | `executionContextFor`, `withLiveSettingsContext` signature, directory/switch closures |
| `cmd/serve_response_run_execution.go` | pass run `sessionID` into the live binding |
| `cmd/serve_handlers_status.go` | extract `runningSessionIDs` |
| `cmd/serve_session_directory.go` | **new** — `sessionDirectory`, query/entry types |
| `internal/tools/session_directory.go` | **new** — read-only tool + `ContextWithSessionDirectory` |
| `internal/tools/live_switch_session.go` | **new** — mutating tool + binding |
| `internal/live/prompts.go` | mention list/switch in voice + execution instructions |
| `frontend/src/stores/live-store.ts` | UX-3 snapshot ownership, `live.session_changed`, `onSessionChanged` |
| `frontend/src/stores/app-store.ts` | UX-1 `selectSession` `keepLive` option, live follow-along, symmetric rebind call |
| `frontend/src/components/LiveStatus.tsx` | UX-2 key on `liveId`, binding line, aria announcement |
| `frontend/src/styles/` | transcript cross-fade only |
| `cmd/serve_live.go` (routes) | `POST /v1/live/sessions/{id}/session` action + handler |
| `frontend/src/api/endpoints.ts` | `liveSwitchSession` route beside the other `liveRoute` entries |
| `docs/live-voice.md` | tools, switch semantics, event list, new route in the endpoint table (`:194-207`) |
| tests | per the test plan below |

### Single verification gate

```sh
gofmt -w <changed-go-files>
make build
go test ./...
go test -race ./cmd -run 'Live'
go vet ./...
npm --prefix frontend run format && npm --prefix frontend run lint \
  && npm --prefix frontend run typecheck && npm --prefix frontend test
```

Use a temp `HOME`/XDG for `go test ./...` per `AGENTS.md`, preserving `GOMODCACHE`.

Optional safety valve, recommended **against** unless asked: a
`live.session_control` config toggle (`internal/config/schema.go`) so switching
can be disabled without a rebuild. It costs config schema plumbing and a second
code path, and the capability is reversible by voice anyway.

## Risks

- Lock ordering (`liveMu` → `record.mu`) must match `startLiveController`; never
  hold either over network I/O (`cmd/serve_live_context.go:55`) or over
  `store.Get`.
- `appendEvent` self-locks — the rebind path needs `appendEventLocked` or it
  deadlocks.
- Any missed lock-free read of `sessionID` is a data race under `-race`.
- Step 4 changes prompt-injection behaviour for abandoned sessions; the §1a test
  is the guard.
- One changeset means no staged production exposure. The concurrency tests in
  steps 6-7 and the `-race` run are doing the de-risking that separate PRs would
  otherwise have done — do not skip them to land faster.
- The tool and the HTTP endpoint must stay two callers of one `rebindLiveSession`
  with one validation path. If validation is duplicated they will drift, and the
  divergence will only show up as voice and UI disagreeing about the binding.
- Optimistic navigation means the UI can briefly be ahead of the server. That is
  intentional and safe — the transcript swap is correct regardless of the binding
  — but the generation guard is what stops a stale response from yanking the user
  backwards.
