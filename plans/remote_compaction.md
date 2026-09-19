# Remote Compaction (Responses API server-side compaction)

Status: design / not yet implemented
Owner: term-llm core (`internal/llm`)
Reference implementation: `codex-rs` (`core/src/compact_remote_v2.rs`, `core/src/compact_remote_v2_attempt.rs`)

## 1. Problem

Today every compaction in term-llm is *local*: `llm.Compact` / `llm.SoftCompact`
run a second, isolated model call that reads the whole transcript and writes a
markdown brief, and the brief plus a bounded raw suffix becomes the new history
(`internal/llm/compaction.go`, `internal/llm/engine_compaction_controller.go`).

That costs a full extra prefill of the context window at exactly the moment the
window is full, throws away prompt-cache locality, and compresses reasoning and
tool state through a lossy text bottleneck.

Newer Responses-API backends expose **server-side compaction**: the server
summarizes the conversation itself and hands back an *opaque encrypted blob*
that stands in for the entire prefix on subsequent requests. This plan adds that
path to term-llm for every Responses provider/model that supports it, with an
automatic fallback to the existing local compaction.

## 2. How codex does it

### 2.1 Capability gating

`codex-rs/model-provider/src/provider.rs`:

```rust
pub enum RemoteCompactionSupport {
    Unsupported,
    /// The provider supports `compaction_trigger` items over the Responses endpoint.
    V2,
}

pub struct ProviderCapabilities {
    // ...
    pub remote_compaction: RemoteCompactionSupport,
}
```

`ConfiguredModelProvider::capabilities()` returns `V2` when the provider is
OpenAI or an Azure Responses deployment; Amazon Bedrock (Mantle/runtime) also
declares `V2`. Everything else is `Unsupported`.

Dispatch happens in exactly two places, both a single `match`:

- manual `/compact` — `core/src/tasks/compact.rs:41`
- inline auto-compaction — `core/src/session/turn.rs:1428`

```rust
match turn_context.provider.capabilities().remote_compaction {
    RemoteCompactionSupport::V2 => run_remote_compact_task_v2(...),
    RemoteCompactionSupport::Unsupported => run_local_compact_task(...),
}
```

A separate `Feature::TokenBudget` short-circuits both and just starts a fresh
window (`core/src/compact_token_budget.rs`) — not relevant to us.

### 2.2 Wire protocol

Request (`compact_remote_v2_attempt.rs:69-87`): an ordinary Responses request —
same instructions, same tool specs, `parallel_tool_calls: true` — built from the
**full current history**, with one extra control item appended last:

```json
{"type": "compaction_trigger"}
```

`ResponseItem::CompactionTrigger {}` serializes to exactly that, with no payload
(`protocol/src/models.rs:1239`, test at `models.rs:3761`). It is a *request
control*, not a durable item: `rollout/src/policy.rs:62` excludes it from
persistence, and `compact_remote_v2_attempt.rs:123` pops it back off before the
input list is reused for history reconstruction.

Response (`collect_compaction_output`, `compact_remote_v2.rs:421-483`): the
stream must yield **exactly one** output item of type `compaction`, then
`response.completed`:

```json
{"type": "compaction", "id": "cmp_...", "encrypted_content": "<opaque>"}
```

`ResponseItem::Compaction` (alias `compaction_summary`; a sibling
`context_compaction` variant with optional `encrypted_content` also exists).
Anything other than exactly one compaction item is a fatal error; a stream that
ends without `response.completed` is a stream error.

### 2.3 New history

`build_v2_compacted_history` (`compact_remote_v2.rs:485-512`):

1. Start from the exact `input` that was sent (minus the trigger item).
2. Keep only:
   - real user messages (`TurnItem::UserMessage | HookPrompt`),
   - client-authored `developer` messages (feature-gated),
   - small inter-agent messages (≤ 10k tokens, excluding subagent progress and
     final answers).
   Everything else — assistant messages, reasoning, function calls, function
   call outputs, web searches — is dropped, because the server blob covers it.
3. Truncate the retained set from the newest backwards to a
   `RETAINED_MESSAGE_TOKEN_BUDGET = 64_000` token budget, truncating the
   boundary message's text (and optionally images) rather than dropping it.
4. Append the `compaction` item as the final history item.

So post-compaction history is literally: `[bounded user-message skeleton...] +
[opaque compaction blob]`. On the next turn that blob is replayed as an input
item and the server rehydrates its own summary.

### 2.4 Safety rails codex ships

- **Pre-trim**: `trim_function_call_history_to_fit_context_window` rewrites/drops
  oversized function-call outputs *before* the compaction request, because the
  compaction request itself still has to fit in the window.
- **Bounded retries**: `MAX_REMOTE_COMPACTION_V2_STREAM_RETRIES = 2`, deliberately
  lower than the normal stream retry budget, since compaction turns are long.
- **Model fallback**: on `context_length_exceeded` / "compact with the current
  model", it retries the whole attempt against a fallback step context (the
  previous turn's model) and records `record_model_fallback`.
- **Blob/model compatibility**: `ModelInfo::comp_hash` identifies which models can
  decrypt a given blob. `CompactedHistoryMetadata.compaction_model_hash` is stored
  with the compacted history so a later model switch can detect an incompatible
  blob instead of sending an undecryptable payload.
- **Never persisted as text**: the compaction item is opaque; UI shows a
  `ContextCompaction` turn item with an empty message string.
- **Token accounting**: `sess.recompute_token_usage(...)` after install; analytics
  record `active_context_tokens_before/after`, `compaction_summary_tokens`.

## 3. What term-llm already has that fits

The Responses stack is closer to this than it looks.

| Need | Existing term-llm mechanism |
| --- | --- |
| Opaque provider item round-trip | `Part{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw}}` (`types.go:328`) |
| Capture of every done output item | `handleOutputItemDone` already records **every** `response.output_item.done` payload verbatim into `h.replayItems` (`responses_event_handler.go:382`) |
| Replay emission | `EventProviderReplay` → engine attaches replay parts to the assistant message (`engine.go:2515`, `engine_run_turn.go:481`) |
| Replay serialization back to the wire | `ResponsesInputItem.Raw` byte-for-byte passthrough (`responses_api.go:146,169`, `buildResponsesAssistantItems`) |
| Redaction from UI/export/review | `withoutProviderReplayParts` (`engine.go:2973`), export test `session/export_test.go:353` |
| Capability flags | `llm.Capabilities` (`types.go:184`) |
| Compaction dispatch point | `runCompactionController.beforeTurn` / `maybeAfterResponse` / `applySoftHardFallback` (`engine_compaction_controller.go`) |
| History replacement + persistence | `CompactionResult`, `Engine.PrepareCompactionContext`, compaction callback, `SQLiteStore.CompactMessages` |

Two consequences worth stating up front:

1. A `compaction` output item **already** survives the round trip today as a
   provider replay part — no new serialization format is required. The work is
   about *requesting* it and *rebuilding history* around it.
2. Because replay parts are opaque and provider-specific, a history whose only
   real content is a compaction blob is **not portable**. Model/provider switching
   after a remote compaction is the single biggest design risk (§6).

## 4. Design

### 4.1 Capability

Add to `llm.Capabilities` (`internal/llm/types.go`):

```go
// RemoteCompaction reports server-side context compaction support. Providers
// that return RemoteCompactionV2 accept a trailing {"type":"compaction_trigger"}
// input item and return exactly one opaque `compaction` output item.
RemoteCompaction RemoteCompactionSupport
```

```go
type RemoteCompactionSupport string

const (
    RemoteCompactionUnsupported RemoteCompactionSupport = ""
    RemoteCompactionV2          RemoteCompactionSupport = "v2"
)
```

Capability alone is not enough — support is per **model**, not per provider. Mirror
the existing `NativeToolDiscoveryProvider` pattern (`types.go:205`), which already
solves exactly this shape:

```go
// RemoteCompactionProvider is implemented only by providers whose transport can
// issue a compaction_trigger request. Support must be exact rather than inferred
// from the Responses API being in use.
type RemoteCompactionProvider interface {
    RemoteCompactionSupport(model string) RemoteCompactionSupport
}
```

Initial declared support:

- `internal/llm/openai.go` — `v2` for models matched by a table in a new
  `internal/llm/remote_compaction.go` (gpt-5.x family + Azure Responses
  deployments), `unsupported` otherwise.
- `internal/llm/chatgpt.go` — `v2` (Codex backend); this is the highest-value path.
- `copilot.go`, `grok.go`, `xai.go`, `opencode_go.go` — `unsupported` until each is
  verified against a live endpoint. Do **not** infer support from "uses
  `ResponsesRequest`".

Config escape hatches under the existing provider config schema:

- `remote_compaction: auto | on | off` (default `auto` = use the table).
- Per-model override so a user can enable a newly shipped model without a release.

### 4.2 Request path

`llm.Request` gains one internal field:

```go
// CompactionTrigger requests server-side compaction: the provider appends a
// trailing {"type":"compaction_trigger"} control item and the turn is expected
// to return exactly one opaque compaction item. Internal orchestration state,
// never user-visible.
CompactionTrigger bool
```

Responses providers translate it in their request builders (the single shared
helper `BuildResponsesInputWithInstructionsAndFilePolicy` callers in
`openai.go:145`, `chatgpt.go:292`): after the input slice is built, append

```go
ResponsesInputItem{Raw: json.RawMessage(`{"type":"compaction_trigger"}`)}
```

Using `Raw` avoids widening `ResponsesInputItem` and guarantees the exact wire
shape. Add a helper `appendCompactionTrigger(items []ResponsesInputItem)` in
`internal/llm/responses_api.go` so all Responses providers share one definition,
with a test asserting the serialized payload is exactly `{"type":"compaction_trigger"}`.

Request invariants for a compaction turn:

- Same `Instructions`, same `Tools`, same `ParallelToolCalls` as a normal turn —
  the server needs the tool surface to summarize tool state faithfully.
- `Ephemeral: false` — unlike local compaction this is a real turn on the server's
  conversation; but `PreviousResponseID`/server-state continuation must be
  **reset afterwards**, see §4.4.
- `MaxOutputTokens` left at the provider default; the summary is not text output.
- No steering, no tool execution: the engine must not dispatch tool calls from a
  compaction turn.

### 4.3 Response path

Add to `responses_event_handler.go`:

- `recordDoneOutputItem` gains `case "compaction", "compaction_summary", "context_compaction":`
  which emits a new internal event:

```go
EventCompactionItem EventType = "compaction_item" // Internal-only opaque server compaction blob.
```

carrying `Event.ProviderReplay` (the verbatim item) plus a parsed marker so the
caller can count occurrences. The generic replay capture at
`responses_event_handler.go:382` still records the raw item, so the existing
assistant-message path continues to work unchanged; the new event exists so the
remote compaction driver can *validate* and *locate* the blob rather than
scraping replay parts by JSON type.

Validation in the driver, matching codex:

- exactly one compaction item → success;
- zero, or more than one → error `errRemoteCompactionUnsupported` /
  `errRemoteCompactionMalformed`;
- stream ends without a terminal `response.completed` → stream error;
- HTTP 400 mentioning `compaction_trigger` (unknown input item type) → treat as
  "provider lied about support", record it, and fall back permanently for this
  session+model.

### 4.4 Driver and history reconstruction

New file `internal/llm/compaction_remote.go`:

```go
// CompactRemote performs server-side compaction and returns a CompactionResult
// whose replacement history is a bounded user-message skeleton followed by the
// provider's opaque compaction item.
func CompactRemote(ctx context.Context, provider Provider, model, systemPrompt string,
    messages []Message, config CompactionConfig) (*CompactionResult, error)
```

Steps:

1. `sanitizeToolHistory(messages)`, then pre-trim oversized tool results using the
   existing `config.MaxToolResultChars` machinery so the compaction request itself
   fits the window (term-llm equivalent of codex's
   `trim_function_call_history_to_fit_context_window`).
2. Stream a request with `CompactionTrigger: true`, the caller's system prompt,
   the caller's tools, and the sanitized history.
3. Collect the single compaction item and the turn `Usage`.
4. Rebuild history:
   - retain `RoleUser` messages and `RoleDeveloper` messages authored by the
     client; drop assistant/tool/reasoning/replay content;
   - truncate newest-first to `remoteRetainedTokenBudget` (start at 64k, clamped
     to `min(64_000, inputLimit/2)` so small-window models are not blown out);
   - append one synthetic assistant message carrying the compaction blob:

```go
Message{Role: RoleAssistant, Parts: []Part{{
    Type:           PartProviderReplay,
    ProviderReplay: &ProviderReplayItem{Raw: compactionItemRaw},
}}}
```

5. Return `CompactionResult{Summary: "", NewMessages: ..., OriginalCount, CompactedCount, Model, Usage}`.

`CompactionResult` gains two fields so downstream layers can tell the paths apart:

```go
Remote               bool   // Server-side compaction; NewMessages ends in an opaque blob.
RemoteCompatibility  string // Provider/model compatibility token for the blob (codex comp_hash analogue).
```

`RemoteCompatibility` is `provider + "/" + compactionModelFamily(model)` for now;
if a backend later exposes a real hash, swap the value without changing the shape.

### 4.5 Controller integration

In `internal/llm/engine_compaction_controller.go`, replace each direct `Compact(...)`
call site (`beforeTurn:203`, `maybeAfterResponse:187`, `applySoftHardFallback:142`)
with a single dispatcher:

```go
func (c *runCompactionController) compact(messages []Message) (*CompactionResult, error) {
    if c.remoteEnabled() {
        result, err := CompactRemote(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, messages, *c.config)
        if err == nil {
            return result, nil
        }
        c.disableRemoteIfPermanent(err)
        slog.Warn("remote compaction failed; falling back to local", "error", err)
    }
    return Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, messages, *c.config)
}
```

`remoteEnabled()` = provider implements `RemoteCompactionProvider`, returns `v2`
for `c.req.Model`, config does not force `off`, and no permanent failure has been
recorded for this run.

Soft compaction interacts badly with the remote path: the brief-injection dance
(`beginSoft`, `contextContinuationBriefPrompt`, `ToolChoiceNone`) exists to get a
*text* brief out of the main model. When remote compaction is available, **skip
soft compaction entirely** and compact remotely at the soft threshold — one server
turn replaces the brief turn. Concretely: `beforeTurn` checks `remoteEnabled()`
first; if true and `softReached`, run the remote compaction and `apply` it,
never setting `softInjected`.

`apply()` already does the right things (restores discovery replay, calls
`PrepareCompactionContext`, fires the compaction callback,
`resetProviderConversation`, zeroes `lastTotalTokens`, sends `EventCompaction`).
`resetProviderConversation` matters especially here: after installing a
server-summarized history we must *not* continue from `previous_response_id`, or
the server will re-expand the very prefix we just compacted.

### 4.6 Persistence and UI

- Session persistence already stores `Part.ProviderReplay` raw JSON, so the blob
  survives resume with no schema change. Confirm round-trip in a
  `internal/session` test.
- The compaction boundary message has **no text**. `SQLiteStore.CompactMessages`
  and the transcript renderer must tolerate an empty `TextContent` assistant
  message; render it as the existing compaction separator ("Context compacted")
  rather than an empty bubble. Check `internal/session/transcript.go` and the
  TUI/web transcript paths.
- Store `CompactionResult.RemoteCompatibility` on the session row (new nullable
  column `compaction_compat TEXT`, via `internal/sqliteutil` migration) so resume
  and model switching can validate it.
- Export/markdown must keep excluding replay parts — already covered by
  `TestExportToMarkdownNeverExportsProviderReplay`; add a remote-compaction case.

## 5. Phasing

| Phase | Scope | Exit criteria |
| --- | --- | --- |
| 1 | Capability plumbing: `RemoteCompactionSupport`, `Capabilities.RemoteCompaction`, `RemoteCompactionProvider`, model table + config override, no behavior change | unit tests for the support table and config precedence |
| 2 | Wire: `Request.CompactionTrigger`, `appendCompactionTrigger`, `compaction` output item handling + `EventCompactionItem` | `responses_api_test.go` asserts exact trigger JSON; handler test drives a scripted SSE stream with one compaction item |
| 3 | `CompactRemote` + history reconstruction + retention budget | table-driven tests over retention/truncation; malformed-response error cases |
| 4 | Controller dispatch, soft-compaction bypass, fallback-to-local | engine harness tests: remote success, remote 400 → local fallback, exactly-one-compaction enforcement |
| 5 | Persistence, transcript/UI rendering, session resume, compat token + migration | `internal/session` round-trip and render tests |
| 6 | Enable for `chatgpt` + `openai` gpt-5.x by default; leave others off | manual verification against live endpoints, documented in `docs-site/content` |

Phases 1–3 are independently mergeable and inert; behavior only changes in phase 4.

## 6. Risks and open questions

1. **Portability of compacted history.** After a remote compaction the transcript
   is a user-message skeleton plus an opaque blob. Switching to Anthropic, or to a
   model with a different compatibility token, loses essentially all context and
   may hard-error (`Could not decrypt the provided encrypted_content` — term-llm
   already has a grok test for that error shape, `grok_test.go:603`). Mitigations,
   in order of preference:
   - store `RemoteCompatibility` and, on mismatch at turn start, run a **local**
     compaction of the *pre-compaction* history if it is still available;
   - otherwise warn the user explicitly at model-switch time;
   - never silently send an incompatible blob.
   This needs a decision before phase 4 ships.
2. **Local summary safety net.** Codex keeps nothing but the blob. We could
   optionally run the existing local brief in parallel (cheap model) purely as a
   portable fallback. Costs a second call; probably a follow-up, not v1.
3. **Retention budget vs. small windows.** 64k retained tokens is fine for 272k
   windows and absurd for 128k. Clamp as described in §4.4 and test both.
4. **`store=false` providers.** `opencode_go.go` sets `Store: false`; whether
   server-side compaction is even meaningful there is unverified. Keep it
   unsupported until proven.
5. **WebSocket transport.** `ResponsesClient` can run over WebSocket
   (`UseWebSocket`, `WebSocketServerState`). Compaction turns are long; codex
   disables websockets in its remote-compaction tests. Decide whether to force
   `ForceHTTP: true` for compaction requests (recommended for v1).
6. **Tool-call-in-flight.** A compaction triggered mid-turn while tool calls are
   pending must not orphan a `function_call` without its output. The existing
   `sanitizeToolHistory` handles the local path; verify it also runs on the
   remote request input.
7. **Cost/usage attribution.** The compaction turn's `Usage` must be attributed to
   compaction, not to the user turn, so session metrics stay comparable with the
   local path.

## 7. Test plan

- `internal/llm/responses_api_test.go`: trigger item serialization; trigger is
  appended last and only when `CompactionTrigger` is set.
- `internal/llm/responses_event_handler_test.go`: SSE with one `compaction` item →
  one `EventCompactionItem` plus a replay part; zero and two items → errors.
- `internal/llm/compaction_remote_test.go`: retention/truncation table; blob is the
  final message; assistant/tool content dropped; usage propagated.
- `internal/llm/engine_test.go` (harness + `MockProvider`): remote path chosen when
  capability present; local path when absent; local fallback on remote error, with
  the fallback recorded once and not retried on every turn.
- `internal/session/`: replay part round-trip through SQLite; empty-text compaction
  boundary renders as a separator; export still redacts.
- Regression guard: a test asserting that a provider **without** the capability
  never receives a `compaction_trigger` item.
