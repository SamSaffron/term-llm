# `/side` on claude-bin: a promotable side lane over a shared boundary

## Problem

`/side` runs on claude-bin today, but answers are context-starved. claude-bin
declares `Capabilities.ManagesOwnContext` (`internal/llm/claude_bin.go:347`), so
`Engine.ConfigureContextManagement` leaves `inputLimit` at 0
(`internal/llm/engine.go:918`), and `InputLimitForProviderModel("claude-bin", …)`
is also 0 (no `InputLimit` on the claude-bin entries in
`internal/llm/models.go:131-150`, no prefix row for bare `opus`/`sonnet`/`fable`/
`haiku` in `inputLimitTable`). Both `/side` call sites pass that zero
(`internal/tui/chat/side_question.go:163-167`, `cmd/serve_side_question.go:343-347`),
so `sidequestion.BuildMessages` falls to `fallbackSideInputLimit` and builds the
request inside a **6,400-token** budget. On a real claude-bin session the whole
transcript is trimmed away, silently, with no error.

Separately, replaying a large transcript as stdin for every side question is the
expensive way to do this on a CLI provider that already holds the conversation.

Goal: `/side` works at any point in time on claude-bin — idle or mid-run — and
reuses the live Claude Code session when it can, without a second mechanism
living beside the existing ones.

Longer-term goal this design must not foreclose: a side discussion runs for a few
turns and then the user says *"let's start a session about this"* and it becomes a
real branch. Today that is not merely unimplemented — branching actively discards
side questions (`m.clearSideQuestionHistory()` in
`internal/tui/chat/tree.go:771`). The structure below is chosen so promotion is a
later addition rather than a rewrite.

## What is already shared, and what is not

- `/fork` is a **session** branch: it copies transcript rows into a new term-llm
  session and relaunches (`internal/tui/chat/tree.go:211`,
  `createConversationBranch`). It does **not** copy provider state, so a forked
  session's claude-bin provider starts cold and replays the transcript through
  `buildStreamJsonInput`'s `<conversation_history>` path. `/fork` shares nothing
  with `--fork-session`.
- What `/fork` and `/side` genuinely share today is the **boundary**:
  `runboundary.Tracker` (`internal/runboundary/tracker.go`) exposed through
  `MainRunManager.ActiveBoundary` — `/fork` takes `DurableAnchorID` from it
  (`tree.go:162-169`), `/side` takes `Messages` from it
  (`side_question.go:56-61`). On the web side the equivalent single point is
  `rt.refreshSideQuestionSnapshot` (`cmd/serve_side_question.go:121`).
- The **provider fork** seam already exists and is used by handover:
  `helperConversationForker` / `forkConversationProvider`
  (`internal/llm/helper_provider.go:9-32`), with claude-bin's implementation at
  `claude_bin.go:263-276` and the two-path request shape at
  `compaction.go:1645-1676` (fork → `system + policy + trigger`; else → bounded
  replay via `buildHelperRequestMessages`).

So the plan adds **two** concepts — a boundary that carries provider state, and a
side lane that owns one — and extends **two** existing seams. It does not
introduce a side-question-specific transport.

## The side lane

A side question is not a one-shot request; it is a turn in a **lane** that can
later become a session. A lane is:

- an **anchor**: the `runboundary.Snapshot` it forked from, including
  `DurableAnchorID`/`Durable` — already carried today (`tracker.go:14-20`) and
  already what `/fork` branches on (`tree.go:162-169`);
- its **own transcript**, as `[]llm.Message` rather than flattened
  `{Question, Response}` pairs;
- its **own provider**, kept alive across lane turns, and its exported state after
  each answer.

The first side question forks from the main boundary. Every later lane turn
resumes the **lane's** session — for claude-bin, `--resume S2 --fork-session`
delivering only the new question, with no history re-rendering at all.

Three consequences matter:

1. **Follow-ups get structurally correct and cheaper.** No flattening of prior
   side answers into text, because the lane's own session holds them.
2. **The concurrency risk shrinks to the first turn.** Resuming a session the
   parent CLI process still holds mid-turn — the Phase 0 probe question — applies
   only when the lane is created. Every later turn resumes a session nobody else
   owns.
3. **Promotion becomes additive**, using storage APIs that already exist:
   `branchStore.CreateBranch(sourceSessionID, {AnchorMessageID: lane.anchor.DurableAnchorID})`,
   write the lane transcript into the child, then
   `SaveProviderState(childSessionID, providerKey, lane.providerState)`
   (`session.ProviderStateStore`, `internal/session/store.go:926-930`). For
   claude-bin the promoted session then *continues* its CLI session instead of
   replaying — strictly better than `/fork`, which copies no provider state today.

**Open UX decision.** An anchored lane stops seeing main-conversation progress
after its anchor. That is correct branch semantics for something promotable, but
it differs from today's behaviour, where every side question re-reads the latest
boundary. Proposed default: anchor on the first question, re-anchor automatically
while the lane still holds a single exchange, surface the anchor in the panel
header, and require an explicit re-anchor after that. Decide before the lane data
model hardens.

**New cost.** A live clone across turns is a lifecycle object, not data. It needs
cleanup on panel close and clear, invalidation on undo/redo, compaction and model
swap, and the web `backup`/`restore`/`close` paths now carry a provider handle
rather than plain state. Reload is fine: the clone is not serializable, but its
exported state is, which is what the seam exists for.

## The one new concept: provider state travels with the boundary

A boundary becomes `(messages, providerState, workingDir)` captured at the same
instant. `ExportProviderState` already serializes exactly what a fork needs for
every CLI provider (`claude_bin.go:288`, plus grok-bin/cursor-bin/agy-bin), and
`RetryProvider` already forwards it (`retry.go:142-155`).

Capturing at the existing capture sites — and only there — is what makes this
safe and non-duplicative:

- TUI: `runboundary.Tracker.Commit` is the single point where a provider-complete
  turn is recorded (`tracker.go:92`, called from
  `internal/tui/chat/streaming.go:236`). Export there.
- Web: `rt.refreshSideQuestionSnapshot` advances the web snapshot
  (`serve_run_callbacks.go:79`, `serve_run_finalize.go:67`, `serve_runtime.go:1931`,
  `serve_skills.go:1064`) and invalidates it (`serve_undo_redo.go:39,57`,
  `serve_run_history.go:45`, `serve_compaction.go:113`,
  `serve_run_persistence.go:365`). Extend its signature so state cannot be
  refreshed without the messages it belongs to, or invalidated without dropping
  the state.

The web side has four further paths that move this state and must be handled
explicitly, not by patching fields independently:
`initializeSideQuestionSnapshot` (`serve_side_question.go:105`), `backup`/`restore`
(`:150`, `:161`, used by replace-history rollback in `serve_run_history.go:42`),
`close` (`:225`), and `updateSideQuestionConfig` (`:83`), which rewrites
provider/model metadata and must not leave new identity attached to an old
snapshot.

Because eligibility depends on provider and model identity, the captured value is
`(messages, providerState, workingDir, providerKey, model)` — one value with one
owner, not separate fields.

This also removes the current race risk: claude-bin's `sessionID`,
`messagesSent` and `transcriptDigest` are plain fields written during streaming
(`claude_bin.go:1150`, `533-535`) and read by `forkHelperConversation`
(`264-275`). Handover gets away with it by only forking when idle; `/side` cannot.
Capturing at `Commit` means the state is read on the run's own goroutine at a
boundary, never concurrently.

## Why the claude-bin side of this is nearly free

claude-bin already implements "resume a session and deliver only what is new":

- `buildArgs` emits `--resume <sessionID>` (+ `--fork-session` when
  `forkSession`) for non-ephemeral requests (`claude_bin.go:1439-1444`).
- `Stream` computes the delta as `req.Messages[p.messagesSent:]` and filters
  assistant/tool turns through `claudeResumeDelivery`, because the resumed session
  already holds them natively (`claude_bin.go:473-479`, `1354-1363`).
- `resumeBoundaryValid` verifies the delivered prefix against
  `transcriptDigest` and, on mismatch, resets and replays in full
  (`claude_bin.go:449-453`, `1336-1347`).

So the fork request is: clone with state `{sessionID, messagesSent: N, digest}` +
`forkSession = true`, and `Messages = boundary.Messages + [policy, question]`.
`Stream` then delivers `boundary.Messages[N:]` (assistant/tool → filtered out) plus
policy + question. `--resume … --fork-session` supplies the context; the system
prompt still travels on argv (`systemPromptForTurn`, `1599`).

No new request path inside `claude_bin.go`.

### Two fork contracts, not one

There are two genuinely different fork shapes, and they must stay separate:

- **Suffix-only** (today's `forkHelperConversation`, `claude_bin.go:263-275`):
  resume the session and deliver *only* the messages handed to the clone. It
  zeroes `messagesSent` and `transcriptDigest` on purpose, and handover relies on
  it — its forked request is just `[system, policy, trigger]`
  (`compaction.go:1654-1661`).
- **Boundary-offset** (new, for `/side`): resume the session and treat
  `requestMessages[:N]` as already delivered, per the captured state.

Collapsing these would break handover: importing a parent state with
`messagesSent = N` into a 2-3 message request makes `resumeBoundaryValid` fail
(`claude_bin.go:1340-1342`), which resets the clone, drops `--resume`, and sends
policy + trigger with **no conversation history at all**. So the existing
suffix-only path stays exactly as it is; the new seam is added beside it. Both are
a few lines on top of `cloneHelperProvider`, so there is no real duplication to
remove. Note that this therefore does *not* fix handover's live-field read; that
is pre-existing and out of scope, and handover only forks when settled.

### The seam validates and refuses

`ForkHelperAtBoundary(provider, state, requestMessages)` returns `(Provider, false)`
unless, inside the provider, `state.MessagesSent <= len(requestMessages)` and
`digest(requestMessages[:MessagesSent]) == state.Digest` (with
`MessagesSent > 0 && Digest == ""` — legacy state accepted as unverifiable by
`ImportProviderState`, `claude_bin.go:319-322` — treated as ineligible here).

Refusing is what makes the fallback safe. If the clone were allowed to discover
the mismatch itself, `Stream` would reset and replay the **untrimmed** boundary as
fresh stdin (`claude_bin.go:449-453`), which is exactly what the budget work in
Phase 1 exists to prevent. Refusing routes the caller to the ordinary bounded
replay instead.

### The invariant, not an ordering assumption

The design does not depend on exporting provider state at a particular instant.
It depends on one invariant: **the captured state is never ahead of the captured
messages.** Being behind is always safe — the delta simply covers more ground and
`claudeResumeDelivery` filters what the session already holds.

This holds naturally because `messagesSent` only ever counts messages the provider
was actually given, and a published boundary always includes those. It is also
why the paths that fire `TurnCompletedCallback` *without* a provider-state advance
are harmless: steering injection (`engine.go:1619`,
`engine_turn_completion.go:296,545`) and the error-recovery callbacks
(`engine_turn_recovery.go:134,188,267`) publish messages with the previous turn's
state, which is behind, so the delta absorbs them.

Where the invariant could break is a rejected or out-of-order `Commit`
(`runboundary/tracker.go:98`) leaving the boundary behind the provider. The seam's
`MessagesSent <= len(requestMessages)` check catches exactly that and falls back to
replay, so no separate defence is needed — but a test must cover it.

## Decisions that need code (not just wiring)

1. **`Ephemeral` and `SessionID` belong to the lane, not to `/side`.**
   `sidequestion.Run` hard-sets `req.Ephemeral = true` and clears `SessionID`
   (`internal/sidequestion/sidequestion.go:468-469`). Both must become lane
   properties, because a promoted lane is by definition non-ephemeral with a real
   session id. This is the most dangerous change in the plan: for the Responses
   providers (ChatGPT, OpenAI, Copilot, Grok) `Ephemeral` is what prevents a
   helper turn from chaining onto the live conversation, so non-ephemeral must be
   structurally inseparable from "this provider came from the fork seam" — never a
   convention. Every other restriction `Run` applies (no tools, no search,
   `MaxTurns` 1, no multi-agent) stays unconditional. A regression test must
   assert the parent provider's `sessionID`, `messagesSent` and
   `transcriptDigest` are unchanged after a lane turn, and that the lane reports
   a different CLI session id.
2. **`WorkingDir` must be propagated.** Claude Code keys sessions by project
   directory and `prepareClaudeCommand` passes `workingDir` to `cmd.Dir`
   (`claude_bin.go:766`). Main runs set it (`streaming.go:998`,
   `serve_runtime.go:1980`); `/side` sets nothing today, so a fork launched from a
   different cwd would not find the session. Capture it with the boundary.
3. **Lane history needs no special shape on the fork path.** Prior side answers
   are `RoleAssistant` messages and `claudeResumeDelivery` drops them — which is
   correct, because the lane's own session already holds them. Only the **replay**
   path, where there is no lane session, must carry prior exchanges explicitly;
   it keeps today's alternating messages. Flattening prior answers into a text
   block is explicitly rejected: it is lossy and would make a promoted session
   inherit fabricated structure.
4. **Lane transcript shape.** `sidequestion.Entry`
   (`{Question, Response, CreatedAt, Usage}`) is a flattened pair. The lane keeps
   `[]llm.Message` plus per-turn usage so promotion can write real messages; the
   existing `Entry` shape stays only where it is already persisted in reload state
   and the web view (`internal/tui/chat/process_reload.go:48`,
   `cmd/serve_side_question.go:53`).
5. **Eligibility is one function**, not a condition scattered across two call
   sites: fork only when the side provider key equals the main provider key, the
   provider implements the fork seam, the captured state is non-empty, and the
   state belongs to the snapshot being used. Otherwise replay.
6. **Cost/cleanup.** Creating a lane is one extra `claude` process and one new CLI
   session; later lane turns reuse it. The parent prefix should hit Anthropic's
   prompt cache, which is the point. Lane teardown must run on panel close, panel
   clear, and every invalidation listed for the boundary.

## Open question that gates the design (probe first)

Does `claude --resume <id> --fork-session` work while the parent process still
holds `<id>` mid-turn, and does the forked session already contain the in-flight
user turn? With the lane model this applies **only to lane creation**; later lane
turns resume a session no other process owns.

- If it works and the on-disk session is **not** yet ahead of our boundary:
  fork at any time, deliver the computed delta.
- If it works but the session **is** already ahead: fork at any time and deliver
  only policy + question for mid-run questions, so the in-flight user turn is not
  delivered twice.
- If it fails: fork only when no run is active, replay mid-run. `/side` still
  works at any point in time; only the cheap path narrows.

This must be answered by a live probe before Phase 2 is written, and the answer
recorded in this document.

## Phases

**Phase 0 — probe (needs authorization).** Live claude-bin probes for the three
outcomes above, plus confirmation that `InputLimitForProviderModel("claude-bin", …)`
is 0 in a real process. Record results here.

**Phase 1 — helper input budget for self-managed providers.** This removes the
unintended 6,400-token fallback; it does not remove trimming. A large claude-bin
transcript will still be trimmed, just against a realistic budget. Add
`llm.HelperInputLimitForProviderModel` (or equivalent) that falls back to a
per-provider-type assumption for `ManagesOwnContext` CLI providers, and use it in
`sidequestion.BuildMessages`. Deliberately do **not** change
`InputLimitForProviderModel`: it feeds the TUI context meter
(`internal/tui/chat/stats.go:62`), `/models` output (`cmd/models.go:407`), the
web context-usage endpoints, and `/compact`'s helper config
(`internal/tui/chat/commands_skills.go:113-118`, `cmd/serve_compaction.go:60-65`),
all of which are intentionally absent for claude-bin today. This phase alone
fixes `/side` quality on claude-bin, grok-bin, cursor-bin and agy-bin and is
independently shippable. Tests: budget resolution per model/effort suffix; a
large snapshot survives instead of collapsing to policy + question.

**Phase 2 — boundary-offset fork seam.** Add
`forkConversationAtBoundary(state []byte, requestMessages []Message) (Provider, bool)`
to `internal/llm/helper_provider.go` with a `ForkHelperAtBoundary` free function;
implement it on claude-bin by reusing `cloneHelperProvider` +
`ImportProviderState` + the validation above + `forkSession = true`; forward it on
`RetryProvider`. **Leave `forkHelperConversation` and the handover path untouched**
— the two contracts are different (see above). Tests use the existing
`commandRunner` override to assert `--resume`, `--fork-session`, the stdin delta
and untouched parent state without spawning `claude`, plus refusal on
offset-past-end, digest mismatch, and empty-digest legacy state. Add a handover
regression test asserting its forked request still resumes with a parent offset
greater than its own message count.

**Phase 3 — boundary carries state.** `runboundary.Snapshot` gains
`ProviderState`, `WorkingDir`, `ProviderKey` and `Model`; `Tracker.Commit` takes
the state; **`Reset`/`New` take a pre-stream captured state** so the first
provider turn of a run can still fork from the previous run's session (the
worked example depends on this); `InvalidateDurable` leaves state alone, since
state validity is decided by the seam, not by durability. Wire the TUI `Commit`
site and every web path listed above. Tests: state never published ahead of its
messages; undo/redo, replace history, compaction and close drop it;
backup/restore round-trips the whole tuple; a provider/model swap makes the
boundary ineligible.

**Phase 4 — the lane, owned in one place.** Introduce `sidequestion.Lane`: it
holds the anchor, the transcript, the provider and its exported state, and
decides per turn whether to create itself (fork or replay) or continue. Lane
creation with a fork issues **request messages `anchor.Messages + [policy,
question]`**; the stdin delta is derived by the provider, so request shape and
wire shape are deliberately different. A continuing lane turn issues
`laneTranscript + [question]` against the lane's own session. Collapse the
duplicated ~20-line prelude in `internal/tui/chat/side_question.go:155-205` and
`cmd/serve_side_question.go:320-382` onto the lane, and give the TUI
`SideQuestionState` and the web `sideQuestionRuntime` a lane rather than loose
fields. Tests: fork chosen when eligible; replay when the provider key or model
differs, when there is no state, when the seam refuses, or when a run started
between capture and preparation; a second lane turn resumes the lane session
instead of re-forking; parent provider untouched; lane torn down on clear, close
and every boundary invalidation; mid-run behaviour per Phase 0.

**Phase 5 — promotion (designed for, not built here).** "Start a session about
this" becomes: `branchStore.CreateBranch(sourceSessionID,
{AnchorMessageID: lane.anchor.DurableAnchorID})`, write the lane transcript into
the child session, then `SaveProviderState(childSessionID, providerKey,
lane.providerState)`. No new storage APIs
(`session.ConversationBranchStore`, `session.ProviderStateStore`). The line that
must change when this lands is `m.clearSideQuestionHistory()` in
`internal/tui/chat/tree.go:771`, which currently discards lanes on branch. Phase 4
must leave the lane serializable enough for this; nothing here needs to be built
now.

**Phase 6 — verification.** `gofmt`, `make build`, `go test ./...`, `go vet ./...`,
plus `go test ./internal/llm -run Claude` and the side-question suites in
`internal/tui/chat` and `cmd`. Frontend is untouched.

## Worked example

TUI, `claude-bin:opus-max`, working dir `W`. Transcript indices are the
`llm.Message` list the engine sends.

### Main turns 1-2 (no side question yet)

| Index | Message |
| --- | --- |
| 0 | system (term-llm prompt) |
| 1 | user "Refactor the widget loader." |
| 2 | assistant A1 (text + `edit_file` tool call) |
| 3 | tool T1 (result) |
| 4 | assistant A2 "Extracted `loadWidget` into `widget_loader.go`." |
| 5 | user "Now add a test." |
| 6 | assistant A3 (tool call) |
| 7 | tool T2 |
| 8 | assistant A4 "Added `TestLoadWidget`." |

Provider state timeline inside the single live `ClaudeBinProvider`:

- Turn 1 `Stream(req.Messages = [0,1])`: no `--resume`; CLI reports session `S1`.
  Afterwards `messagesSent = 2`, `transcriptDigest = D([0,1])`.
- Turn 2 `Stream(req.Messages = [0..5])`: `--resume S1`; delta `[0..5][2:]` =
  `[A1,T1,A2,user5]`, filtered by `claudeResumeDelivery` to `[user5]` — only the
  new user turn crosses stdin. Afterwards `messagesSent = 6`,
  `transcriptDigest = D([0..5])`.
- `Tracker.Commit` for turn 2 captures
  `boundary = { Messages: [0..8], ProviderState: {S1, 6, D6}, WorkingDir: W }`.

### Turn 3 starts, then `/side` mid-run

User sends "Now handle the nil case." → index 9. `Tracker.Reset` publishes
`Messages: [0..9]` with the run-start state `{S1, 6, D6}` (still the honest
boundary: the CLI provably received the transcript through index 5).

`/side what file did the loader move to, and is the nil case covered?`

1. `cmdSide` reads the boundary — messages `[0..9]`, state `{S1, 6, D6}`, dir `W`.
2. `sidequestion.Prepare` eligibility: side provider key `claude-bin` == main
   provider key, provider implements the fork seam, state non-empty, boundary
   matches the snapshot → **fork**.
3. `ForkHelperFromState(provider, {S1, 6, D6})` returns a fresh
   `ClaudeBinProvider` clone: `sessionID = S1`, `messagesSent = 6`,
   `transcriptDigest = D6`, `forkSession = true`, same model/effort/env/OAuth, no
   tool executor, no MCP server. The live provider is not touched.
4. Request: `Messages = [0..9] + [policy(developer), question(user)]` (12),
   `Ephemeral = false`, `WorkingDir = W`, `Tools = nil`, `MaxTurns = 1`.
5. Inside `Stream`: `resumeBoundaryValid` → `6 <= 12` and `D([0..5]) == D6` ✓.
   Delta `Messages[6:]` = `[A3,T2,A4,user9,policy,question]` → filtered to
   `[user9, policy, question]`.
6. argv: `--print --output-format stream-json --include-partial-messages
   --verbose --strict-mcp-config --disable-slash-commands --setting-sources ""
   --dangerously-skip-permissions --settings {"disableAllHooks":true}
   --max-turns 1 --model opus --tools "" --resume S1 --fork-session
   --system-prompt <main system prompt> --input-format stream-json`, `cmd.Dir = W`.
   No `--mcp-config`, so the forked session has no tools at all.
7. stdin: one user message carrying `user9`'s text, the `<developer>` policy
   block, and the question. Turns 1-2 come from the resumed session, not stdin.
8. Events stream back as `EventTextDelta` into the side panel; `usage` from the
   `result` line lands in `stats` under `side_question`. `CleanupMCP` runs on the
   clone; the clone and its new CLI session are then garbage.

Probe dependency, and the contract it settles: a captured session id is **not** a
frozen conversation. `claudeBinProviderState` holds only an id, an offset and a
digest (`claude_bin.go:278-281`), so `--resume S1` later loads whatever `S1`
contains *then* — possibly `user9`, partial assistant output, or a completed
turn. The digest cannot see this; it describes our local prefix, not Claude's
session file.

The contract chosen here is **latest native context, delivered from the provably
sent offset**: the fork may see more than our boundary, and we re-deliver only
what we know crossed stdin. That can duplicate one user line; it can never lose
one. Delivering from `len(boundary.Messages)` would invert the trade — no
duplication, but silent loss whenever the CLI had not yet received a turn — which
is worse for a question usually asked *about* the in-flight turn.

Phase 0 decides whether the duplication occurs at all, and whether concurrent
resume of a live session is safe. If it is not, mid-run falls back to replay and
forking is restricted to an idle boundary — enforced against a run starting
during fork startup, not checked once.

### Follow-up question: the lane continues

Lane creation produced its own CLI session `S2`. The follow-up does **not**
re-fork from the main boundary; it resumes the lane:

- argv: `--resume S2` (no `--fork-session`; the lane owns `S2`), same
  `--tools ""`, `--max-turns 1`, `--system-prompt`, `cmd.Dir = W`.
- stdin: one user message containing only the new question. The prior exchange is
  already in `S2`, and the policy was delivered when the lane was created.

Each lane turn is therefore cheaper than the last, and the lane accumulates the
exact state promotion later needs. Only the replay path — no lane session — has
to carry prior exchanges explicitly.

### Promotion, later

`/side` → "start a session about this" branches at `lane.anchor.DurableAnchorID`
(the boundary index 8 row, in this example), writes the lane's real messages into
the child session, and saves the lane's exported `S2` state under the child
session id. The new session then continues `S2` on its next turn rather than
replaying a transcript.

### `/side` while idle

No active run, so there is no tracker boundary. The snapshot is
`m.buildMessages()` and the state is exported live from `m.provider` at that
moment — safe, because nothing is streaming. Everything else is identical; the
delta is just `[policy, question]` because `messagesSent` already equals the
transcript length after the last completed turn.

### Fork ineligible → replay

Cases: first turn of a brand-new session (no `sessionID` yet), a model or
provider swap since the boundary, provider state dropped by undo/redo, replace
history or compaction, or a state that does not cover the snapshot. Then
`Prepare` returns the replay path: `Ephemeral = true`, no `--resume`, full
stream-json stdin with the `<conversation_history>` wrapper, trimmed to the
Phase 1 helper budget. Same panel, same events, same stats bucket.

### Digest mismatch

If the transcript prefix was rewritten after the state was captured — or the
boundary fell behind the provider — the seam validates, refuses, and `Prepare`
returns the bounded replay path instead. The clone is never allowed to discover
the mismatch itself, because its own recovery is an untrimmed stdin replay. No
parent state is affected either way.

### Lifecycle, unchanged from today

Generation counter invalidates late events, `Esc` cancels via the stored
`context.CancelFunc`, `Ctrl+X` clears panel history, the panel transcript never
enters the session, usage is attributed to `side_question`, and the main run's
own stream, approvals and steering are untouched throughout.

## Rejected alternatives

- **Hand the live provider to `/side`.** Races on claude-bin's unguarded session
  fields and risks corrupting the parent's resume boundary.
- **Copy provider state into `/fork` branch sessions.** A separate feature; the
  Phase 2 seam enables it later, but it is out of scope here.
- **Teach `sidequestion` about claude-bin.** Would duplicate CLI transport
  knowledge outside `internal/llm`; the seam keeps it in the provider.
- **Collapse the handover fork and the `/side` fork into one implementation.**
  Attempted and rejected during review: the contracts differ (suffix-only vs
  boundary-offset) and merging them silently strips handover's context.
- **Keep side questions stateless, re-forking from the main boundary each time.**
  Simpler per-turn, but it forecloses promotion: there is no accumulating lane
  session, no lane provider state, and prior answers have to be flattened into
  text. It also keeps the live-session concurrency risk on *every* side turn
  instead of only on lane creation.
- **Add `InputLimit` to the claude-bin model table.** Simplest edit, but it
  silently changes four unrelated surfaces at once (Phase 1 note).
