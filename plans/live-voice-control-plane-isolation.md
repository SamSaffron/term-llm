# Keeping the control plane out of chat sessions

## Revision 3 — SHIPPED. Read this first; both designs below are superseded.

The `control:` sentinel of Revision 2 lasted about an hour in production before it
failed the same way the JSON did, twice:

1. `"Switch to the attachment icons one"` — the voice model paraphrased instead of
   prefixing, and the host-side prose guard missed it because the request named no
   session (the user was pointing at a directory entry the assistant had just read
   out).
2. `"Do you mind switching back to the session where I was managing session with
   voice"` — the guard's word list knew `switch` but not `switching`.

Both are the same failure: **a hand-written rule is a word list, and a word list has
a bottom.** Asking the voice model to recognise a category it has no stake in, and
then bolting on a regex to catch it when it doesn't, is the thing that was wrong —
not the particular format.

What shipped instead is a chained-LLM router:

```
delegation ──▶ fast LLM (4 tools) ──▶ handles it, or pass_to_workspace ──▶ session lane
```

- **Every delegation is triaged** by one cheap fast-model turn with the three
  control tools plus a host-local `pass_to_workspace(request)`. It either does the
  session management itself and answers, or hands the request over — optionally
  tidied, since it is reading the text anyway.
- **No sentinel, no prose guard, no classification by the voice model.** The voice
  prompt lost its entire control bullet; one clause of the work bullet now says
  these requests travel the same channel in the user's own words, with nothing to
  prepend.
- **Routing fails open.** An absent router, a router error, or a router timeout
  sends the *original* text to the session lane, so a triage outage degrades to the
  pre-control-lane behaviour rather than losing or refusing work. This is the
  property that makes replacing a deterministic guard with a model safe.
- **The handoff short-circuits**: `pass_to_workspace` records and cancels its own
  turn, so the common path costs one provider request rather than two.
- Routing runs on the FIFO route worker, off the controller event loop.

Deleted: `internal/live/control_prose.go` and its tests, `ClassifyDelegation`, the
sentinel, `controlUsageText`, `NOT_CONTROL` — about 670 lines net.

Everything below is retained as the design record. The reasoning still holds — why
the lane must exist, why it must branch in the controller rather than in `Run`, how
tool authority is built without a run, and why chatgpt cannot host tools. Only the
mechanism for *recognising* a control request was overturned, twice.

Status: proposal / brainstorm (no code changed)

## The problem, observed in the wild

Asking voice to "swap to the Discourse Backport Angel Widget session" produced
this row in a later `session_directory` search:

```json
{"session_id":"2026-09-17T10-56-09-52f879","number":8386,
 "title":"Manage sessions with voice","running":true,
 "snippet":"Um, can you swap to the discourse backport angel widget session"}
```

The control-plane utterance became that chat session's **visible transcript
content**, its sidebar snippet, and an FTS-indexed message. The session's own
search result is now a request to leave it.

It is worse than cosmetic. Every control-plane request currently:

- persists a user turn + assistant turn into the bound session's durable transcript;
- burns tokens on that session's model (often a large coding model, to answer
  "which sessions are running?");
- increments `UserTurns` / `LLMTurns` / `ToolCalls` / token metrics, distorting
  session stats;
- extends the `previousResponseID` chain, so the provider-side conversation of a
  real work session carries control chatter;
- pollutes the session it **departs from** — so a switch A→B dirties A, and
  switching back dirties B. Two switches dirtied two unrelated sessions.

## Why it happens

The realtime voice model is deliberately given exactly **one** tool:

- `openAIDelegationTool = "delegate_to_controller"` (`internal/live/openai_call.go:19,79`)
- `geminiDelegationTool = "delegate_to_controller"` (`internal/live/gemini_protocol.go:9,159`)

and the event parsers hard-reject any other name
(`internal/live/openai_events.go:119`, `geminiDelegationInput`). So *every*
spoken request — including pure control-plane ones — is funnelled through
`serveLiveDelegator.Run`, which starts an ordinary stateful chat turn in the
bound session:

```go
req := s.buildResponsesLLMRequest(..., sessionID, true)
options := startResponseRunOptions{previousResponseID: ..., uiSession: true, live: d.live}
s.startResponseRun(runtime, true /*stateful*/, false, []llm.Message{message}, req, sessionID, options)
```

The control tools are then attached to that turn (`liveControlTools`). The
architecture says "voice's only verb is delegate", so control-plane work has
nowhere else to go. That was the right call when the only capability was
"do some work here"; it stops being right once voice gained verbs that are
*about* sessions rather than *within* one.

## Options

### A. Host-executed realtime control tools — recommended

Give the realtime model a **second tier** of tools that the host executes
in-process and answers directly, with no delegated chat turn at all:

| Tier | Tools | Executed by | Touches a chat session |
| --- | --- | --- | --- |
| Delegation | `delegate_to_controller` | the bound session's runtime | yes (correctly) |
| Control | `session_directory`, `live_switch_session`, `live_settings` | the serve host, in-process | **no** |

Decision rule, stated once and easy to hold: **host-local, deterministic, no
workspace → realtime tool. Needs reasoning, files, shell, or project context →
delegate.**

These three qualify exactly. They are already call-scoped, already have no
workspace access, already deterministic, and already implemented as plain Go
functions behind a context binding. Routing them through an LLM turn in a coding
session adds latency, cost and pollution while adding nothing.

The mechanism already exists on the OpenAI path:

- `Tools` is a slice (`internal/live/openai_call.go:77`), so adding declarations
  is additive.
- `sendFunctionOutput` (`internal/live/openai_provider.go:236-244`) already emits
  `function_call_output` + `response.create`, which is precisely how a
  host-executed tool result is returned to a realtime model.

What changes:
- declare the control tools in the provider session payload;
- stop hard-rejecting non-delegation tool names in the event parsers; emit a new
  normalised event kind (`EventControlCall`) carrying name + arguments;
- `Controller` routes `EventControlCall` to a new host interface
  (`ControlExecutor`) instead of the `Delegator`, and returns the JSON result via
  the existing function-output path;
- `cmd` implements `ControlExecutor` over the *same* `sessionDirectory` /
  `switchLiveSession` functions the tool and HTTP endpoint already share — a
  third caller of one validated path, not a reimplementation.

Wins: zero transcript pollution anywhere; no token cost on the session model;
much lower latency (one model turn instead of two); the voice model still
narrates naturally because it receives the tool result; no classification
failure mode, because the model picking a tool *is* the classification.

Cost: the realtime tool surface grows from one to four. That was a deliberate
constraint, so it needs an explicit, documented exception — justified because
these tools cannot touch a workspace, cannot run code, and are call-scoped.

### B. A second delegation scope + an ephemeral control lane

Keep one delegation verb but add a scope (`{input, scope: "session"|"control"}`),
or declare a sibling `delegate_to_control`. Control-scoped delegations run in a
**non-persisted lane**: an ephemeral runtime with only the control tools, no
workspace, a small fast model, and no writes to any durable transcript.

There is a strong in-tree precedent for exactly this lane shape — **side
questions** (`internal/sidequestion/`, `cmd/serve_side_question.go`):

- a parallel execution path attached to the runtime with its own bounded history
  (`HistoryLimit = 20`);
- never written to the main transcript;
- surfaced separately in stats ("Private Side-Question History",
  `cmd/serve_stats_endpoint.go:318-324`);
- explicitly does not masquerade as a main run
  (`TestSideQuestionDoesNotMasqueradeAsMainRun`, and `hasActiveSideQuestion` is
  a distinct predicate from `hasActiveRun`).

The lifecycle pattern is right. The policy is not: side questions are
deliberately **tool-less** (`SystemPolicy`: "You have no tools and cannot
inspect files, run commands, search, delegate, or take actions"), whereas the
control lane's entire purpose is to call tools. So this is a sibling lane
modelled on side questions, not a reuse of them.

Worth keeping as the **fallback for providers that cannot host extra realtime
tools**, and for genuinely compound requests ("find the reflow session and
summarise what we decided") where reasoning is required across the directory
result.

### C. Host-side post-hoc classification — reject

Inspect the delegated turn and retro-actively decide it was control-plane. This
cannot work: you only learn the intent *after* the model has run and chosen
tools, and by then the user message has already been persisted into the session
transcript. The damage is done before the signal exists.

### D. Run every delegation ephemerally, promote on workspace touch — reject

Too invasive for the benefit. It breaks steering (`Run`→`Steer` depends on a
registered active run), transcript continuity, and `previousResponseID`
chaining, to solve a problem that only affects a small, identifiable class of
requests.

## Recommendation (revised after verifying the protocol)

**B as the primary and only correctness mechanism; A later as a per-provider
latency optimisation.** The chatgpt frameless protocol has no tool concept at all
(see the section below), so A cannot be the general answer. B works identically
on every provider and fixes the pollution at its source — the branch happens
before `startResponseRun` is ever reached.

## The gating feasibility question — ANSWERED: chatgpt cannot host tools

Verified against `internal/live/events.go` and `internal/live/chatgpt_*.go`.

**`grep -rn "Tools\|tool" internal/live/chatgpt_*.go` (excluding tests) returns
nothing.** The chatgpt provider has no tool concept at all. Its session payload
is just `{sdp, session}` (`chatgpt_call.go:63-64`); there is no `Tools` field to
extend.

The frameless protocol's full surface (`internal/live/events.go`):

| Direction | Types | Tool support |
| --- | --- | --- |
| Inbound (`ParseEvent`) | `session.started`, `session.updated`, `input_transcript.added`, `output_transcript.added`, `turn.done`, `delegation.created`, `session.closed`, `error` | **no function-call event exists** |
| Outbound (`Outbound`) | `delegation.context.append`, `session.context.append`, `session.update`, `session.close` | **no `function_call_output` equivalent** |

The *only* server→client request channel is `delegation.created`, gated to
`item.type == "delegation" && item.target == "client"` (`events.go:124-131`), and
it carries only free `input_text` content (`delegationInputText`). The delegation
contract is provider-native — the backend already knows about client
delegations; the host never declares anything.

So **option A is unavailable on the provider actually in use.** OpenAI Realtime
(`sendFunctionOutput`, `openai_provider.go:236`) and Gemini
(`FunctionDeclarations` slice) could support it, but chatgpt cannot, and the
design must not be provider-conditional in its *behaviour*.

### Consequence: B becomes the primary design

Because the classification cannot ride on a declared tool argument on chatgpt, it
must ride **inside the delegation input**, and the host must branch **before**
`startResponseRun` is ever called — which is exactly where the pollution
originates, so the fix lands in the right place anyway.

Uniform shape across providers:

1. **Marking.** Voice instructions (`internal/live/prompts.go`) tell the model to
   mark control-plane requests. On providers where we declare the tool we can add
   a real `scope: "session"|"control"` argument; on chatgpt it must be a sentinel
   convention in the input text. Either way `internal/live` normalises it into
   one field:
   ```go
   type DelegationScope string
   const (ScopeSession DelegationScope = "session"; ScopeControl DelegationScope = "control")
   // DelegationRequest gains Scope.
   ```
   Host behaviour is then identical on every provider; only the parsing differs.

2. **Routing.** `serveLiveDelegator.Run` branches on `request.Scope` before
   touching the bound session:
   ```go
   if request.Scope == live.ScopeControl {
       return d.runControlDelegation(ctx, request, emit)   // never calls startResponseRun
   }
   ```

3. **The control lane.** Ephemeral, modelled on side questions: control tools
   only, no workspace, a small fast model from live config, bounded in-memory
   history per call, nothing persisted, and **not registered as an active run**.

4. **Fail-safe direction.** A misclassified *work* request reaching the control
   lane finds no workspace tools and fails cheaply, and can be retried. A
   misclassified *control* request reaching the session lane is exactly today's
   behaviour, so there is no regression floor below the status quo.

Option A stays on the roadmap as a per-provider **latency** optimisation for
OpenAI/Gemini once the lane exists, not as the correctness mechanism.

## Revision 2 — superseded by Revision 3 above.

The JSON wire format below was built and then replaced, because it failed in
production for the reason the plan never priced in: **a voice model will not
reliably emit exact JSON.** Observed behaviour was that it paraphrased the
request into an ordinary delegation instead, which the prose guard then refused.
The guard was doing all the load-bearing work and the wire format none of it.

What shipped instead:

- **Sentinel is `control:`** (case-insensitive prefix) followed by **plain
  English**. No JSON, no tool name, no ids. Emitting this is a bar the voice
  model can actually clear.
- **A session assistant runs the request**: a cheap fast-model turn
  (`llm.NewFastProvider`, the same convention as auto-titling) with a small
  *static* system prompt, exactly the three control tools, `SetAllowedToolsFilter`,
  `MaxTurns: 4`, `Ephemeral: true`, ~30 s bound, and no runtime, session, or
  persistence. It resolves "the reflow session" itself by calling
  `session_directory` and then acts, which deletes the lookup-then-switch
  choreography the voice model previously had to perform.
- **The prose guard now routes instead of refusing.** Since the lane accepts
  natural language, a forgotten prefix simply works. False positives are handled
  by the assistant replying exactly `NOT_CONTROL`, which the host converts into a
  refusal telling the voice model to resend without the prefix.
- **Control answers run on a dedicated worker goroutine**, not on the controller
  event loop. This is new and non-negotiable: the old lane was a ~1 ms in-process
  tool dispatch, and running a multi-second model turn in its place would stall
  transcript deltas and barge-in for the duration.
- The compact directory projection (`liveControlDirectoryText`) is gone — it
  existed so the *voice model* would not read kilobytes of tool JSON; the
  assistant reads it now and speaks a summary.

Everything below is retained because the *reasoning* still holds — why the lane
must exist, why it must branch in the controller rather than in `Run`, how
authority is built without a run, and why chatgpt cannot host tools. Only the
payload format and the "no LLM in the lane" conclusion were overturned.

## Final design (superseded by Revision 2 above)

**B2 — no LLM in the control lane.** The voice model emits a tool call over the
prose channel; the host parses it strictly and invokes the existing `Execute`
functions directly. No model turn, no runtime, no ephemeral lane to maintain.
A malformed payload is bounced to the *voice model*, not to a fallback LLM and
not to the session lane.

### Correction: classify in the Controller, not in `Run`

Branching in `serveLiveDelegator.Run` is **too late**. `handleEvent` routes
`EventDelegationCreated` straight to `enqueueDelegation`
(`internal/live/controller.go:244`), and `runSteeringDelegations` documents that
"New requests are always offered to Steer first, even while a previous Run
blocks" (`controller.go:355-358`), calling `Steer(ctx, request)` on pending items
before it will ever start a `Run`. So a control request during an active task is
injected as guidance before `Run` is consulted.

Classification therefore happens in `internal/live` at `EventDelegationCreated`,
and control requests execute **synchronously there**, bypassing
`enqueueDelegation`, `runSteeringDelegations`, and `runWithBusyRetry`'s
15 × 2 s busy retries (`controller.go:436`, `:77-79`).

### Wire format

Parse only `request.Input` — the text the *model* authored. Never parse
`TranscriptDelta`, which carries raw user speech (`controller.go:313`).

```
#control {"tool":"live_switch_session","arguments":{"session":7447}}
#control {"tool":"session_directory","arguments":{"query":"reflow","running_only":true}}
#control {"tool":"live_settings","arguments":{}}
```

Parser rules, fail-closed:

- Trimmed input not starting with `#control` → `ScopeSession`, untouched. This is
  the only fail-open path and it is exactly today's behaviour.
- Input starting with `#control` → `ScopeControl` **unconditionally**. A
  `#control`-prefixed string must never reach `startResponseRun`; persisting
  model-authored wire syntax into a transcript is worse than the bug we are
  fixing.
- The remainder must be exactly one JSON object: `json.Decoder` with
  `DisallowUnknownFields` over `{tool, arguments}`, then `decoder.More()` must be
  false (trailing prose = malformed). Cap the payload (~2 KiB).
- `tool` must be in a fixed three-name allowlist. `arguments` defaults to `{}`
  and is passed verbatim to `Execute`, which already validates it.
- Malformed → execute nothing, reply as commentary explaining the rejection and
  the correct form, complete the delegation. One voice turn, zero cost, nothing
  persisted anywhere.

### Authority in a lane with no run

The control tools pin authority to `llm.SessionIDFromContext(ctx)`. There is no
run and no engine in the control lane, so the host builds the context itself:
the bound session id via `llm.ContextWithSessionID` plus the existing tool
bindings. Only the host can construct it, so nothing can forge it, and the tool
implementations stay unchanged.

### Output budget

`session_directory` results must stay small: appends chunk at
`MaxAppendBytes = 500` (`events.go:52`) and the delegation writer truncates at
`delegationHeadBytes = 4000` / `delegationTailBytes = 2000`
(`controller.go:79-80`). Use a compact control-lane projection (number, title,
running, short snippet).

### Remove the safety net deliberately

Stop attaching `liveControlTools` to session turns and drop the control-tool
sentences from `ExecutionInstructions`. Keeping them as a fallback makes the
failure mode *silent* (pollution again) instead of *loud* (the coding model says
it cannot switch sessions and the user repeats). Loud is correct while the
prompt beds in.

Also: `live_switch_session`'s description says the switch takes effect "on the
next request". In the control lane it is **immediate** — no chat turn is in
flight to finish. Update the wording.

### Push the directory to the model (removes most lookups) — DEFERRED

Not implemented in the first cut. It changes what the voice model sees at call
start, which is a separate behavioural change worth its own testing, and the
feature is coherent without it because the instructions tell the model to look
up first. Keep as the next follow-up.


After call start and after every successful switch, send a compact directory as
`session.context.append` commentary. "Swap to the Discourse Backport session"
then resolves in the voice model to a single `#control` switch with no lookup
round trip, which removes most compound requests and most malformed-output risk,
because the model no longer has to recall a number it has not seen.

### Forward compatibility with option A

`{tool, arguments}` is already the shape of a function call, so on OpenAI/Gemini
a real `function_call` maps onto the same executor with zero translation: both
parsers produce `ControlRequest{Tool, Arguments}`.

### Dropped from this plan

The `parent_id` durable-control-session trick. Under B2 there is no LLM
reasoning to audit, the exchange is already visible in the live panel, and it
would create a throwaway session per call.

## A second, current bug the control lane fixes for free

`serveLiveDelegator.Run` checks for an active run before doing anything:

```go
if activeID := s.ensureResponseRuns().activeRunID(sessionID); activeID != "" {
    admitted, err := d.Steer(ctx, request)
    ...
    emit(... "Guidance queued for the running task.")
```

So **if the bound session has a task running and you say "switch to session
7447", the switch request is injected as steering guidance into that running
task instead of switching the call.** Same for "what's running?" — it becomes
guidance, not an answer.

Because a control-scoped delegation bypasses this check entirely (it is not work
in the bound session and is not an active run), the control lane makes session
management work *while* a task is running — which is precisely when a user is
most likely to want to look around and move elsewhere.

## If a durable control session is wanted anyway

The user's instinct was "its own side session". If auditability matters more
than zero footprint, create one control session per live call and **parent it to
the bound session**. Three invariants then fall out of existing code for free:

- `ExcludeSubagents` is literally `parent_id IS NULL`
  (`internal/session/sqlite_sessions.go:922-923`), so it is hidden from the
  sidebar **and** from `session_directory`;
- the switch path already refuses parented sessions, so the control plane can
  never bind the call to its own scratch session;
- it inherits normal retention/cleanup.

That said, the control exchange is **already visible** in the live panel's own
voice transcript (`frontend/src/components/LiveStatus.tsx`), which is where
control chatter belongs. A durable chat session would mostly duplicate it.
Recommend starting with no durable session, and adding one only if an audit
requirement appears.

## Secondary cleanups this exposes

- **Model selection.** Control work currently runs on the bound session's model.
  Even in option B, the control lane should use a small fast model from live
  config, not opus/codex, to answer "what's running?".
- **Stats.** If any control work keeps running as a turn, it must be excluded
  from session `UserTurns`/`LLMTurns`/token metrics the way side questions are,
  or session stats stay distorted.
- **Voice history seeding.** `liveSessionOptions` seeds the voice conversation
  with a tail of the bound session's messages. After a switch, that tail is from
  the *previous* session. Independent of this work, but the same family of bug.

## Open decisions

1. ~~Verify the chatgpt frameless protocol can host extra tools~~ — **answered: it
   cannot.** B is the primary design.
2. **Marking convention for chatgpt.** A sentinel prefix in the delegation input
   is the only channel available. Pick something the model will emit reliably and
   a user would never say verbatim, and make the parser strict so a false
   positive cannot silently divert real work.
3. Durable control session, or in-memory only? (Recommend in-memory; the live
   panel already shows the exchange.)
4. Does `live_settings` join the control lane? (Recommend yes — same shape, same
   pollution.)
5. Should a control-scoped delegation be allowed while the bound session is
   busy? (Recommend yes — that is the bug fixed for free above.)
