# Side Questions: Overlay Rework Specification

Status: implemented
Prepared: 2026-07-16
Updated: 2026-07-17
Reworks: PR #924 (`feat/side-conversations`)

## 1. Decision

Replace persistent side conversations with **ephemeral side questions presented over the current conversation**.

A side question is a one-turn, tool-less inference that runs concurrently with the main turn. It receives a snapshot of the latest completed main context plus a small private rolling history of earlier side questions. Its question and answer never enter the main transcript. The user remains visually and conceptually in the main conversation.

This follows Claude Code's `/btw` model rather than maintaining a second live conversation.

The central product rule is:

> `/side` is an overlay, not a place.

## 2. Goals

- Let the user ask a clarification while the main model continues working.
- Keep the main conversation visible; never switch to a separate transcript merely to ask a side question.
- Refresh side context from the latest completed main state on every invocation.
- Preserve enough private side history for follow-up clarifications.
- Prevent side questions from mutating shared state.
- Keep side content out of the main transcript, provider resume state, compaction input, search, exports, and session message counts.
- Remove the stale-branch ambiguity of PR #924's persistent side model.

## 3. Non-goals

- A second independently navigable chat runtime.
- A durable side transcript.
- Continuous synchronization with a currently streaming main answer.
- Tools, MCP, shell commands, approvals, subagents, file mutation, or external actions from a side question.
- Multiple simultaneous side questions for the same main session.
- Merging a side answer back into the main transcript automatically.
- Promoting a side exchange into another session; copy it or restate the conclusion in main.
- Supporting follow-up turns inside one side request. A follow-up is another `/side` request.

## 4. User model

### 4.1 Command

```text
/side <question>
```

Examples:

```text
/side Which column is authoritative during the dual-write phase?
/side Does that change the rollback checkpoint you mentioned?
```

`/side` with no argument opens or reopens the overlay with its dedicated side composer focused. It does not send a request. The composer placeholder is `Ask a follow-up…`.

Remove these commands from PR #924:

```text
/main
/side close
```

There is no side location to return from or close.

### 4.2 Lifecycle

1. The main answer may be idle or streaming.
2. The user enters `/side <question>`.
3. A side overlay appears over or adjacent to the still-visible main conversation.
4. The main run continues unchanged in the background.
5. The side answer streams into a compact chronological transcript. Each exchange is rendered as the `You` question followed by the Markdown `Side` answer.
6. When the answer completes, a pinned, single-line side-only composer appears for follow-up questions.
7. The user can dismiss the overlay and remains at the same main scroll position and input state.
8. Each follow-up is a fresh one-turn request using newer completed main context and prior private side history.

### 4.3 Overlay controls

TUI:

- While answering, the composer is hidden and the footer shows `Esc cancel`. `Esc` cancels only the side request, keeps the overlay open, and restores the side composer.
- While idle, `Esc` clears a non-empty side draft first; with an empty draft it closes the overlay.
- `Enter` sends the single-line side draft. The side composer has no attachments, slash commands, model picker, tools, multiline growth, queue, or nested side mode.
- `PageUp`/`PageDown` or `Ctrl+Up`/`Ctrl+Down`: scroll the chronological transcript. Ordinary cursor keys remain available for text editing.
- `Ctrl+C`: copy the latest answer.
- `Ctrl+X` twice: clear private side history.

Web:

- The same chronological transcript and pinned side-only composer are shown.
- `Enter`/Send submits the side draft. While answering, the composer is hidden and `Esc cancel` is shown.
- `Esc` follows the same cancel, clear-draft, then close sequence as the TUI.
- Copy latest and confirmed Clear controls are provided.
- Side answers use the shared sanitized rich-Markdown renderer.

The main transcript must remain visible behind the overlay. Opening, sending from, cancelling, or dismissing the overlay must not change the active session, route, transcript scroll anchor, or main input draft. A side request cannot be queued or started while another side request is active.

## 5. Context semantics

### 5.1 Main snapshot

Each invocation captures the main conversation's latest **completed, provider-safe context** at the moment `/side` is submitted.

The snapshot must:

- include completed user, assistant, and complete tool-call/result turns available to the main model;
- include the current system/developer/user context required to interpret the conversation;
- exclude the currently streaming incomplete assistant message;
- exclude dangling tool calls or results;
- strip cache anchors and transient UI/runtime metadata;
- not change after the side request starts.

Reuse and rename `session.PrepareForkContext` as a general provider-safe context snapshot helper. It must operate on the main model's in-memory context, not reconstruct the snapshot from SQLite, because the in-memory state is authoritative during an active turn.

If the main answer completes while a side request is running, that completed answer is **not** injected into the in-flight side request. The next `/side` invocation sees it.

### 5.2 Private side history

Keep up to 20 successful, non-synthetic side exchanges per active main session:

```go
type SideQuestionEntry struct {
    Question   string
    Response   string
    CreatedAt  time.Time
    Usage      llm.Usage
}
```

A new side request receives:

1. the fresh completed-main snapshot;
2. earlier side question/answer pairs in chronological order;
3. the current side question.

This creates a lightweight clarification thread while still refreshing from main on every question.

History rules:

- append only a completed, non-synthetic answer;
- cap at 20 entries by dropping the oldest;
- do not append cancelled, empty, or transport-failed requests;
- synthetic errors may be displayed but are not context for later questions;
- clear history on `/new`, `/clear`, resume into another session, runtime teardown, or explicit `x`/Clear;
- do not write history to the session message store or any durable side table.

History is scoped to the active session runtime, never process-global across unrelated sessions.

### 5.3 Side system policy

Use a short, explicit side-question instruction rather than PR #924's mutation-approval policy:

```text
This is a private side question about the current conversation.
Answer it directly in one response.
The main conversation continues independently.
You have no tools and cannot inspect files, run commands, search, delegate, or take actions.
Use only the supplied conversation and side-question history.
If the answer is not available there, say so.
Do not promise future action.
```

Provider-native system/developer roles should be used where supported. Otherwise prepend the policy to the side question using the provider's established fallback.

## 6. Execution semantics

### 6.1 Independent request

A side question uses an independent request/stream and cancellation context. It must not reuse or cancel the main request's context.

It should inherit from the main runtime:

- provider and provider key;
- model and reasoning mode;
- system/developer configuration needed to understand the main transcript;
- current working-directory description as context only.

It must not inherit executable capabilities:

- no term-llm tools;
- no MCP servers;
- no provider-native search;
- no approvals;
- no subagents or queued agents;
- no shell or filesystem actions.

Defense in depth is required:

1. construct the side request with an empty tool registry and no MCP configuration;
2. set provider-native tool/search controls off;
3. enforce a one-turn limit in the engine path;
4. reject any emitted tool call rather than executing it;
5. convert an attempted tool call into a synthetic answer explaining that side questions cannot use tools.

The current side-specific approval and shell policy added by PR #924 should be removed. A tool-less runtime is simpler and safer than maintaining a second permission regime.

### 6.2 Concurrency

- Main and side streams may run concurrently.
- Only one side request may run per main session.
- Submitting another side request while one is active returns `A side question is already running` and focuses the existing overlay.
- Cancelling or dismissing the side request must not affect the main request.
- Main ask-user/approval attention must remain visible in the overlay header so the user knows the main run is blocked.
- Session shutdown cancels both requests and waits for bounded cleanup.

### 6.3 Usage and accounting

Side usage is billable and must not disappear from accounting merely because its messages are private.

- Emit ordinary provider usage telemetry tagged `source=side_question`.
- Exclude side tokens from the main conversation's context-window calculation.
- Show side usage separately in `/stats` and web usage details when available.
- Do not create transcript messages merely to persist usage.
- If the existing global usage recorder is durable, record side usage there with the parent session ID and source tag; otherwise retain it in runtime history for the session lifetime.

## 7. TUI architecture

Replace `ConversationHost` and multiple full `chat.Model` runtimes with one main `chat.Model` containing a small side-question controller:

```go
type SideQuestionState struct {
    Visible    bool
    Running    bool
    Question   string
    Response   strings.Builder
    Synthetic  bool
    History    []SideQuestionEntry
    Composer   textarea.Model
    Cancel     context.CancelFunc
    Generation uint64
}
```

The overlay renders all successful history entries in chronological order and appends the active exchange while it streams; it does not page through a selected entry.

Recommended responsibilities:

- `side_question.go`: context construction, request startup, cancellation, event reduction, history.
- `side_overlay.go`: rendering and key handling.
- `commands.go`: `/side` parsing only.
- the existing main stream loop remains authoritative for the main conversation.

Side stream messages need their own generation ID so late events from a cancelled request cannot populate a later overlay. They do not need general conversation routing IDs because there is only one full chat model.

Delete or revert:

- `internal/tui/chat/conversations.go` and its multi-runtime routing machinery;
- `/main` navigation;
- parent/side runtime status switching;
- inactive-runtime rendering and image-sequence routing;
- side-session resume behavior.

## 8. Web architecture

Keep the current main session route and transcript mounted. The overlay owns one chronological transcript plus a pinned side-only input when idle. Side answers are rendered through the shared sanitized Markdown pipeline; the main composer is never reused for side follow-ups.

Server runtime state:

```go
type sideQuestionRuntime struct {
    mu         sync.Mutex
    running    bool
    generation uint64
    cancel     context.CancelFunc
    history    []SideQuestionEntry
}
```

Attach this to the active server session runtime rather than creating another `serveRuntime` and persisted session.

Proposed private endpoints:

```text
POST   /api/sessions/{id}/side-question
GET    /api/sessions/{id}/side-question
DELETE /api/sessions/{id}/side-question/active
DELETE /api/sessions/{id}/side-question/history
```

`POST` starts a side stream and returns events using the web UI's existing streaming envelope where practical. The endpoint is not OpenAI-compatible and must not be exposed through `/v1/chat/completions` semantics.

`GET` returns active state and in-memory history for browser reconnection to the same live server runtime. A process restart or runtime eviction may legitimately return an empty history.

Remove:

- side-session route switching;
- relationship polling;
- parent/open-side badges and lists;
- `side/reopen` and `side/close` APIs;
- a separately persisted web side transcript.

## 9. Storage and migration changes

PR #924 is unmerged, so redesign its schema directly rather than adding compatibility migrations for an unreleased model.

Remove side-only persistence:

- `side_context_messages`;
- `side_state`;
- the unique-open-side index;
- `SideStore` methods for open/reopen/close/list;
- side-specific session counts and search filtering;
- logic that marks a persisted child as the active side.

`LoggingStore` should not need optional side capability forwarding after this rework.

## 10. Failure behavior

- Empty question: show usage, make no request.
- Main has no completed turns: allow a side question using system context and the user's question.
- Side already running: focus it and show a concise warning.
- Provider error: show error in overlay; do not append history.
- Tool-call attempt: show a synthetic tool-less warning; do not append history.
- Cancellation: keep the overlay open, discard the partial answer, restore the composer, and preserve earlier history.
- Main session changes while side is running: cancel the side before replacing the runtime.
- Provider/model changes: future side requests use the new configuration; existing textual history remains unless the session itself changes.

## 11. Security properties

A side question is read-only by construction, not by asking the model to request approval.

Required tests must prove:

- local tools cannot execute;
- MCP tools are not advertised;
- provider-native tools/search are disabled;
- a hallucinated tool call is never dispatched;
- cancellation does not cancel the main request;
- side content never enters the main transcript, FTS, export, compaction, resume payload, title generation, or message count.

## 12. Test plan

### 12.1 Context tests

- Completed main turns are present.
- The currently streaming assistant message is absent.
- Dangling tool protocol fragments are removed.
- A main turn that finishes after invocation is absent from that request.
- The next side request sees the newly completed main turn.
- Earlier side pairs are included in chronological order.
- History caps at 20.

### 12.2 Runtime tests

- Main and side streams advance concurrently.
- Side cancellation leaves main running.
- Main cancellation leaves side behavior explicitly defined and independent until session teardown.
- Late events from a cancelled side generation are ignored.
- A second concurrent side request is rejected/focuses the first.
- Shutdown performs bounded cleanup.

### 12.3 Isolation tests

- Side question and answer are absent from persisted messages.
- Side content is absent from FTS, export, resume, compaction, and title inputs.
- Side usage is tagged separately.
- Tools, MCP, search, and delegation cannot execute.

### 12.4 TUI tests

- `/side <question>` opens an overlay without changing conversation ID.
- Main transcript remains visible and keeps updating behind the overlay.
- Dismiss restores the same scroll position and input draft.
- Composer focus/send, three-stage Escape behavior, chronological transcript, dedicated scroll keys, copy-latest, clear, and cancel work.
- `/main` and `/side close` are absent.

### 12.5 Web tests

- Starting a side question does not navigate, mutate the main composer, or create a session.
- Chronological side exchanges render through the sanitized Markdown path.
- Side composer focus/send, Escape cancellation/clearing/closing, and concurrent-send rejection work.
- Main and side streams render concurrently.
- Refresh can recover runtime-local history while the server runtime remains alive.
- Runtime eviction/restart safely yields empty history; no side content is persisted.
- Cancel and clear endpoints are idempotent.

## 13. Acceptance criteria

The rework is complete when this story works in both TUI and web:

1. The user has four completed turns about a database migration.
2. Turn five begins generating a deployment runbook.
3. The user asks `/side Which column remains authoritative during dual-write?`.
4. The runbook continues streaming visibly behind the side overlay.
5. The side answers from turns one through four, without tools and without seeing partial turn five.
6. The user dismisses the overlay and is still in the main conversation at the same location.
7. Turn five completes.
8. A follow-up `/side Does that change the rollback checkpoint?` sees completed turn five and the earlier side exchange.
9. Neither side exchange appears in the main transcript or resume state.

## 14. Implementation order

1. Extract provider-safe in-memory context snapshotting and build tool-less one-turn request support.
2. Add side-question controller and history to the single TUI model.
3. Implement the TUI overlay and command semantics.
4. Replace web side-session APIs/runtime with side-question runtime state and streaming.
5. Replace web navigation UI with the overlay.
6. Remove persistent-side schema, stores, runtime host, permission policy, and relationship UI.
7. Rewrite tests and documentation around overlay semantics.
8. Record a new real-model demo using the acceptance story.

Do not preserve the current persistent-side implementation merely for code reuse. Keep only pieces that directly serve the overlay model: safe context copying, independent cancellation, and concurrent event handling.
