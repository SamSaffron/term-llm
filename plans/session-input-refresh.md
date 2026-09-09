# Automatic session prompt/tool refresh — implementation plan

Status: implemented on 2026-09-09. The narrow SQLite transaction, process-local selection coordinator, CLI/web owning-surface integration, staged runtime replacement/admission, explicit lifecycle invalidation, and regression coverage are in place. The historical review disposition below is retained; implementation does not imply a new group-review sign-off.

## Scope and contract

Automatically refresh exactly two things when a durable conversation is first resumed in a process:

1. Its application-supplied system prompt.
2. Its configured local tool selection (`SessionSettings.Tools`), including normal registration/wiring of those tools.

The target is what the normal new-conversation path would resolve for the same agent and surface, with the current invocation's explicit `--system`/`--tools` flags taking their normal precedence. A saved legacy tool selection does not override that target. There is no attempt to infer historical override provenance.

Do not refresh or migrate model/provider, search, MCP selections, approval policy, workspace grants, limits, skill configuration, or other agent settings. Existing startup behavior for these settings stays as-is. Tool names supplied by search, MCP, skills, or request-specific restrictions remain governed by their existing paths; they are not folded into the persisted local tool selection.

No command, button, confirmation, notice message, schema version, configuration fingerprint, pin/follow policy, or general agent reconciliation framework. No model turn or tool execution is triggered by refresh.

Eligibility is explicit at the owning surface: chat TUI resume, ask resume, and first-party web chat-UI requests. Generic runner and compatibility API calls default to ineligible. In web, `isFirstPartyUIResponseRequest(r)` is already evaluated in `cmd/serve_handlers_responses.go` before runtime selection; carry that value into construction rather than waiting for `startResponseRunOptions.uiSession` in `cmd/serve_ui_stream.go`. Do not infer ownership from Agent, Origin, or system-row wording. Generic history hydration, session metadata endpoints, compatibility requests and background delegated runs do not initiate refresh. Borrowed TUI engines reuse their owner's already-prepared selection with no DB write or reset. A human resuming a branch/child through an owning surface is eligible under its own session ID.

Once checked successfully, retain the selected prompt/tools for that session for the process lifetime. Ordinary subsequent turns do no comparison. Runtime eviction, provider-runtime replacement, and TUI switching away/back reuse the selection; a new process makes a new selection. A deliberate fresh conversation, explicit agent switch or successful workspace rebind invalidates the old selection at that control operation. These are existing user-requested context changes, not automatic per-turn checking; an old agent/directory's selection must never overwrite a newly selected one.

## Verified code paths

- `cmd/chat.go:buildChatSessionRuntime` resolves current settings, then overwrites Tools/Search/MCP from the saved session. Tools must stop being overwritten for this operation; Search/MCP restoration remains unchanged.
- `cmd/ask.go:runAsk` resolves settings before discovering the resumed session/agent. Its resume block also restores Tools. Resolve only the desired prompt/tool pair after the durable identity/directory is known; do not replace the other resolved settings.
- `cmd/serve.go:agentRuntimeFactory` / `cmd/runner.go:prepare` already construct current local tools for web runtimes, but the factory does not receive the durable session identity. It needs enough session context to reuse a process-cached pair on recreation.
- `cmd/serve_runtime.go:ensurePersistedSession` hydrates stored history. `RunWithEvents` only injects the runtime system prompt if no system message is already present. Therefore new registered tools do not guarantee new instructions.
- `internal/session/context_state.go:LLMActiveMessages` similarly retains a leading saved system message. Compaction normally strips system messages from the newly persisted active segment and lets runtime instructions supply the prompt.
- `cmd/serve_session.go` already serializes runtime construction and has idle metadata-mutation locks. `cmd/serve_runtime.go` owns `rt.mu` before hydrating history and importing provider continuation.
- `session.ProviderStateStore` stores opaque continuation outside messages. The live provider has an optional `ResetConversation` interface. `Engine.ResetSessionState` is too broad: it also resets tool-owned projections.

## Mechanism

### A. A small process-local selection cache

Add `cmd/session_input_refresh.go` with a testable coordinator instantiated once per process (injectable in tests). Key it by resolved database identity and durable session ID, never display number or runtime pointer. Use `session.ResolveDBPath` for file-backed stores and the unwrapped store's instance identity for isolated `:memory:`/test stores; reset test coordinators between tests. Explicit agent/workspace/new-conversation control operations invalidate the entry as described above. Keep resolved agent/directory on the entry as binding assertions, not a disk-change detector; construction must reject mismatched reuse rather than applying the wrong prompt.

Cache unchanged results too. Each entry holds only binding, selected prompt/tools, and initialization state. Intern identical immutable prompt strings/pairs by exact value; do not assume all sessions of one agent share a prompt (directory/date/includes may differ). Keep the base prompt only where existing skill-decoration code actually needs it. No engine, tool manager, provider or production store is retained. Readiness is intentionally O(number of sessions touched in the process); evicting it would violate the requested contract. Prompt text is O(distinct resolved prompts), with no unbounded duplicate copy per session.

Use an initialization ticket/future, not a mutex held across construction or I/O. Under a short coordinator mutex elect one owner; other callers await its done channel **before acquiring any session-manager/runtime locks**. The owner resolves, constructs, commits and publishes; only then marks ready. Errors/cancellation release the ticket without readiness so retries are possible. Never hold the coordinator map mutex while waiting for engine construction, `rt.mu`, another future or SQLite. Later cache-hit construction applies the selected pair without re-resolving these two fields.

Fresh first-party sessions register their initial pair after successful durable creation/binding. Stateless requests are excluded. Explicit fresh-history replacement with a reused ID discards the former entry. Candidate failure before durable commit leaves the prior runtime/selection usable; the post-commit failure rule below prevents reviving an obsolete runtime. No half-refreshed engine is exposed.

### B. Resolve two outputs, not a replacement settings object

Factor current prompt/configured-tools resolution in `cmd/session.go` into a two-output helper. Preserve current CLI > agent > config precedence. Canonical resumed workspace/worktree and saved agent identity must be available **before** template/include expansion, especially in ask. Keep existing Search/MCP/model/provider/approval restoration unchanged. Reuse normal skill decoration without adding a skill-refresh policy or appending metadata twice.

Select before `SetupToolManager`; actual tool registration and wiring, including spawn/image tools, must use the selected Tools. Never merely replace `Request.Tools` while leaving the old executable registry behind. `cmdRunnerOptions` can carry an internal selected-pair override into `cmd/runner.go:prepare`; it is not a new public run option or a child-run inheritance rule.

Compare exact final prompt strings. For tools, use `tools.ParseToolsFlag`, deduplicate and sort for **comparison only** (`all`/`*` follow that existing parser). If expanded sets match, do not write or invalidate continuation even if ordering/whitespace differs. If different, persist the newly resolved setting string, not a reconstructed expansion; an explicit current empty selection means no configured local tools. Search, MCP, skill tools, tool schemas and request filters are outside this comparison. Date/template changes count as prompt changes; first resume may legitimately pay a full-history replay after such a change.

### C. Narrow durable transaction and capability lookup

Add `SessionInputRefresher` and **`AsSessionInputRefresher`** in `internal/session/store.go`. The latter must unwrap `LoggingStore` like existing `AsStoreChangeStore`/other capability helpers; do not use a raw assertion on the production wrapper. Add `LoggingStore`-wrapped SQLite and wrapped-unsupported-store tests. This avoids falsely advertising the capability on unsupported stores and needs no forwarding method in logger.go. Noop/stateless stores are excluded. An unsupported custom durable store keeps its existing behavior; do not partially refresh memory then re-import stale provider state. Production SQLite must never take that fallback.

Implement in `internal/session/sqlite_session_inputs.go` using existing bounded busy-retry/transaction patterns:

1. Read Tools, compaction state, the earliest transcript row, and the first active row **inside the transaction**. The active row is the first `sequence >= compaction_seq` when `HasCompactionBoundary` applies; if that slice is empty, use the existing full-history fallback semantics. Select first rows, then check role — never search forward for an arbitrary system message.
2. The initial row and active leading row, if system-role, are eligible prompt rows. Deduplicate their IDs. Compare each against the selected prompt and replace only differing text/parts, preserving ID, sequence, creation time and every non-system row. The active leading row is the effective provider prompt; refreshing the initial row too prevents old instructions returning through undo/branch paths. Leave developer/skill context, summaries, attachments, response IDs and boundaries untouched.
3. With **no stored system row**, there is no stored prompt override to repair: use the current runtime fallback. Absence alone is **not** a prompt change and must not delete continuation on every restart. Do not insert/renumber a row or invent a prompt-hash field. This cannot detect an old prompt known only to a provider; that is a stated limit of the narrow two-field policy, not a claim of historical equality.
4. Update `sessions.tools` only for a semantic tool-set change. On an equal-set no-op, return/preserve the saved setting string for metadata so a later Store.Update does not produce a normalization-only write; the executable selection is equivalent. If prompt rows or tools actually change, execute `DELETE FROM session_provider_state WHERE session_id = ?` in this same transaction (all provider keys).
5. Bump `transcript_rev` iff a system row changed. Existing triggers emit transcript changes on that bump and metadata changes on Tools changes. Tools-only refresh therefore emits metadata, not a fabricated transcript edit. Continuation invalidation is applied directly by the refresh owner, not delegated to a transcript watcher. No manual change-log insertion or broad Store.Update/ReplaceMessages.
6. Return changed row IDs/sequences/new parts plus effective Tools. The caller must synchronize all owned copies before later metadata saves. Reread on SQLite retry; do not reuse a pre-transaction compaction boundary.

Expected observable writes: no-op = none; prompt-only = transcript revision/event; tools-only = metadata event; both = both. Provider cache deletion does not count as a conversation turn. In-place replacement deliberately does not archive old prompt editions.

An empty selected prompt replaces old text rather than restoring it. Check existing filtering; if needed, add a narrowly used projection helper that omits an empty refreshed leading system row, without dropping arbitrary later/client system messages. Test initial and legacy-compacted empty prompts.

### D. Exact web construction and admission seam

Use an internal typed factory request rather than another positional parameter or hidden context value:

```go
type serveRuntimeRequest struct {
    SessionID, Provider, Model, Agent, RuntimeDir string
    RefreshInputs bool // first-party owning surface only; default false
    Inputs *sessionInputSelection // selected or cached pair, if eligible
}
```

Change `createRequestRuntime` and the agent-aware factory to take this request. The fallback/no-agent factory delegates with the same request rather than dropping its identity. Thread it from:

- `cmd/serve_handlers_responses.go`: existing `firstParty` classification and resolved `workspaceBinding`, before runtime selection and before `responseServerTools` builds the request list.
- `cmd/serve_session_create.go`: explicit first-party blank creation with its resolved workspace.
- `cmd/serve_model_swap.go`: propagate the originating request's eligibility and cached pair into the candidate; provider/model changes alone do not invalidate selection.
- `cmd/serve_handlers_chat.go` and other compat paths: false; preserve their current factory/settings behavior rather than introducing a new saved-Tools override.
- Metadata-only GetOrCreate and generic/background runner calls: false. `streamUIResponses`/`uiSession` remains a consistency check, not the first point eligibility becomes available.

The handler obtains an initialization ticket outside runtime locks. The factory applies the ticket's selected pair before tool setup. A small staged-preparation hook in the session manager owns installation: initialize candidate engine/tools and normal persisted bindings, commit the narrow transaction, hydrate/synchronize history, then publish the prepared runtime. Mark the ticket ready only after successful publication. Do not run the transaction first and then discover tool construction failed. No import of persisted provider continuation before preparation completes.

If an ineligible request or metadata endpoint already warmed a runtime, the first eligible request must still prepare it. **Choose idle candidate replacement**, not late registry surgery: extend the existing same-agent reuse predicate to require the current prepared-selection identity for eligible requests. Preserve the previous runtime until candidate preparation succeeds; use existing normal restore paths for non-refreshed settings and carry its effective provider/model, search, MCP, limits and approval mode unchanged. If unchanged inputs allow reuse of an existing correctly configured runtime, do not reset it. Compat requests never initiate refresh or create readiness; after a UI refresh they may naturally observe the same session's updated durable state — no promise of two isolated configurations under one session ID.

Pin run admission and installation together. Reuse session operation/reservation mechanisms for staged preparation; do not acquire `lockIdleMetadataMutation` and then recursively call a public replacement method that rejects the same reservation. Add a short admission/identity check in `startResponseRun` so a request holding the retired runtime pointer cannot begin on it after publication. Apply the analogous guard to stateful compat run admission. Busy includes active runs, tools/approvals, side activity and compaction; return existing busy semantics, never cancel those activities to refresh.

Lock ordering: coordinator ticket wait with **no runtime/manager locks** → session operation/admission ownership → brief session-map access → owned runtime mutex if applicable → bounded SQLite transaction. Drop the session-map mutex during construction and I/O. A runtime-held path must never wait on a coordinator ticket or reacquire the session operation lock. Only short, non-waiting ready/failure publication may touch the coordinator while ownership is held. Use a bounded preparation context and release every ticket/reservation on cancellation.

If construction or transaction fails, all surfaces fail that preparation cleanly with an actionable error, no model turn and no ready marker; do not silently use half-new inputs. Reserve/check publication capacity before the transaction. Once the DB commits, prefer finishing the already-reserved publication even if the request disconnects (still no model turn). If publication is impossible, retire/invalidate the old runtime rather than restoring it with old history/provider state, discard the candidate, and leave the ticket retryable. A later preparation is idempotent against the committed target. Never restore obsolete Session.Tools or re-export old continuation after commit. Do not mix this preparation transaction with a speculative model-swap rollback: prepare the source session inputs before BeginSwap, then have both swap candidates inherit the prepared pair.

### E. Runtime history, metadata and continuation

Prefer preparing before TUI scrollback/ask history/serve history is loaded. If a candidate already hydrated, apply returned row changes to its active history under ownership: replace the leading `llm.Message.Parts` with fresh parts (llm.Message has no TextContent field); do not splice user/tool rows or reload over unpersisted input. Set `rt.systemPrompt`, the base/decorated prompt fields as appropriate, `rt.toolsSetting`, `rt.sessionMeta.Tools`, CLI `settings.Tools` and `sess.Tools` consistently. Initialize side-question snapshots afterward. Audit later Store.Update paths and `syncPersistedSessionRuntime`/model-swap rollback snapshots so they cannot write the old Tools back. Add a regression for a post-refresh model swap/metadata save.

A fresh candidate provider has no live continuation; the transaction has removed stale durable blobs before its first normal import. Retiring an idle old runtime closes its provider rather than carrying that provider into the candidate. If a reuse path needs continuation invalidation, call the provider's existing optional `interface{ ResetConversation() }` directly; `RetryProvider` already forwards it. **Do not call Engine.ResetConversation or ResetSessionState for refresh.** This avoids wiping context estimates, pending tool bookkeeping or steering and avoids adding an engine API. Set the selected request/runtime prompt and apply existing persisted context-estimate setup normally. Engine captures the request's system prompt for compaction; there is no new SetSystemPrompt API to invent.

Later turns and eviction/recreation may import/export newly valid provider state normally. Borrowed engines do not write/reset/refresh independently. Concurrent independent processes remain last-writer-wins; each process applies its cached selection locally and does not poll the other's config. Refresh-vs-compaction in the same process is excluded by admission ownership; cross-process transactions reread the committed boundary and never reconstruct history.


### F. Removed tools with historical calls: measured behavior

Live probes on 2026-09-09 used only a synthetic `retired_probe_tool` returning a fixed marker. All four cases passed on claude-bin:fable-medium and chatgpt:gpt-reserve (both HTTP and WebSocket, no HTTP fallback):

1. Complete historical call/result with the tool still declared (control).
2. The same history with no tools declared.
3. The same history with only an unrelated tool declared.
4. A real new probe-tool call, followed on the same provider instance by a request with the tool removed.

Every replay returned the historical marker without a new call or unknown-tool error. The harness asserts Responses conversion retains an actual paired `function_call`/`function_call_output`; it is not silently dropping the call. claude-bin's fresh-history adapter deliberately renders prior completed turns into textual `<conversation_history>`, so this is evidence about claude-bin, not the raw Anthropic Messages API. The WebSocket same-provider case did not show a previous_response_id in its retained last-request state; do not claim that a forced server-side continuation with changed tools was independently verified.

Conclusion: do not retain or re-enable a removed tool merely because completed calls to it exist in history. Preserve the historical call/results and use the new executable tool selection. No grandfather union, replay-only schema registry, or dummy executable tools are justified by these results. Dangling calls without results and other providers are not covered; existing tool-history sanitization/recovery remains unchanged. Refresh happens at an idle boundary, not during a pending tool execution.

Reproducer: `internal/llm/session_refresh_live_probe_test.go`, opt-in via TERM_LLM_SESSION_REFRESH_PROBE=claude-bin or chatgpt, with optional TERM_LLM_SESSION_REFRESH_PROBE_TRANSPORT=http/websocket. Ordinary tests skip it. This is a probe artifact, not refresh implementation.

## Files to touch

### Shared logic and persistence

- **New `cmd/session_input_refresh.go`**: coordinator/tickets, binding assertions, immutable selection data, scoped provider-reset helper and comparison orchestration.
- **`cmd/session.go`**: factor/reuse prompt/tool resolution only; stable store namespace; no other settings migration.
- **`internal/session/store.go`**: optional atomic refresh capability/result types and `AsSessionInputRefresher` decorator unwrapping. Do not assert the capability directly on LoggingStore.
- **New `internal/session/sqlite_session_inputs.go`**: transaction, exact leading-row selection, Tools update, all-provider-cache deletion and conditional revision publication.
- **`internal/session/context_state.go` (conditional)**: only if existing filtering needs a scoped empty-leading-system projection fix.
- **No `internal/llm/engine.go` API change expected**: use the existing provider reset interface when required; never reset the entire engine for this feature.

### Surface integration

- **`cmd/chat.go` / `cmd/ask.go`**: determine durable identity/directory, take selected pair before tool setup, prepare before loading history, synchronize owned Session.Tools; preserve unrelated settings.
- **`cmd/runner.go`**: internal selected-pair override before tool registration/decorating prompt; borrowed engines never initiate refresh.
- **`cmd/serve.go` / `cmd/serve_handlers.go`**: typed factory request and runtime-selection plumbing, default-ineligible fallback factories, prepared-selection reuse predicate.
- **`cmd/serve_handlers_responses.go`**: pass existing early first-party classification and workspace into preparation before tool-spec projection; prepare source before model swap.
- **`cmd/serve_session_create.go`**: register fresh pair only after successful creation/binding.
- **`cmd/serve_model_swap.go`**: carry selection into candidates/rollback metadata without selecting again or reviving stale Tools.
- **`cmd/serve_session.go`**: staged idle preparation/publication under existing operation/reservation/construction mechanisms; no global-map lock during I/O.
- **`cmd/serve_response_runs.go` / `cmd/serve_handlers_chat.go`**: short admission/current-runtime check protecting against retired runtime pointers, for first-party and stateful compat execution. Compat paths never initiate selection.
- **`cmd/serve_ui_stream.go`**: carry/check preparation state; do not first discover eligibility here after tools are already selected.
- **`cmd/serve_runtime.go`**: synchronize prepared history/prompt/Tools and side snapshots before normal state import; no generic hydration-triggered refresh.
- **Existing explicit context-control sites in `cmd/serve_worktrees.go`, `cmd/serve_projects.go` and TUI/agent fresh-session paths**: small invalidation hooks only after successful agent/workspace/fresh-history changes. No per-turn config checking. Keep model-only changes and ordinary runtime eviction cache-preserving.

No frontend, manifest, config schema, migration, user-facing command or approval-system changes. The response-handler files were split after the initial plan; use the current paths above.

### Required tests

1. **7835 regression first** (`cmd/serve_extensions_test.go` / `cmd/serve_runtime_test.go`): use a LoggingStore-wrapped SQLite session with the old prompt/tools; first UI resume gets updated prompt and real shell/view_image/spawn registrations, not merely names. No changes to grants or unrelated settings.
2. **Wrapper capability** (`internal/session/logger_test.go` or a focused capability test): AsSessionInputRefresher reaches wrapped SQLite, unwraps nested decorators, and returns false for wrapped unsupported/Noop stores. Do not accidentally give unsupported stores a success-shaped no-op.
3. **Coordinator** (new `cmd/session_input_refresh_test.go`): no-op/prompt/tools/both, semantic tool-set equality, failed-owner retry, cancellation, concurrent single initialization, bounded waiting, isolated databases/:memory: stores, cache reuse after eviction/switch-away/back, explicit binding-change invalidation, and equal-prompt interning.
4. **SQLite** (new `internal/session/sqlite_session_inputs_test.go`): preserve every non-system row and all IDs/sequences/boundaries; initial vs active-compaction leading-row selection; no arbitrary later system rewrite; text/parts agree; empty and absent prompt; absent prompt does not delete provider blobs on every fresh coordinator; all provider keys deleted only on real changes; rollback and stale-boundary retry. Assert no-op=zero writes/events, prompt-only=transcript, tools-only=metadata, both=both, with no duplicate log entries.
5. **CLI** (`cmd/chat_resume_test.go`, `cmd/ask_resume_context_test.go`): current explicit tools/system precedence, correct resumed agent/worktree expansion, Search/MCP/provider/model/approval unchanged, no duplicate system prompt, context-estimate baseline retained.
6. **Mixed web access** (`cmd/serve_runtime_test.go`, Responses/Chat Completions tests): compat-only requests do not refresh or mark ready; compat-warmed runtime followed by first UI request is safely replaced/prepared; UI then compat does not trigger another selection; borrowed TUI/background runners do not independently refresh.
7. **Admission/races** (`cmd/serve_test.go`, `cmd/serve_response_runs_test.go`): simultaneous first requests, eviction during preparation, active tools/approvals/side work/compaction, stale pointer attempting admission after replacement, cancellation at each stage, and commit-success/publication-failure retry. No deadlock, no active-run cancellation and no half-initialized registry used.
8. **Continuation/metadata** (`cmd/serve_runtime_test.go`, `cmd/serve_model_swap_test.go`): old all-key blobs absent before first import, warm retired provider never re-exported, fresh continuation remains usable after recreation; prepared source/candidate/rollback retain Tools; later metadata/status/title save cannot revert them. No engine-wide resets or context-estimate loss.
9. **Projection/compaction** (`internal/session/context_state_test.go`, `skill_provenance_test.go`, relevant TUI tests if touched): selected prompt survives compaction; skill/developer context and human conversation preserved; empty-prompt projection scoped correctly.

## Implementation order and verification

1. Add the failing wrapped-store 7835 test and wrapper capability tests before implementation.
2. Implement the narrow transaction, two-output resolver and coordinator/ticket tests.
3. Wire CLI resume, then typed web factory construction and staged preparation/admission. Do not implement two alternative factory approaches.
4. Wire explicit invalidation/model-swap propagation, then run mixed-surface, compaction, continuation and metadata-writer regressions.
5. Run `go test ./internal/session ./internal/llm ./internal/tui/chat ./cmd`, targeted `go test -race` on coordinator/wrapper/admission tests, `go build ./...`, and `git diff --check`.
6. Use synthetic sessions only; never mutate live 7835 as a test. Review the diff for changes outside these two fields. Live retirement probes need not be rerun for plan hardening.

## Review disposition

Fable approved the preceding revision and reviewed the live probe. The requested group round produced Gemini and Opus conditional approval, Muse rejection pending concrete fixes, and a substantive Grok critique before timeout (not a completed approval). The present amendments address verified findings; do not label them unanimous reviewer approval.

Accepted and specified: production wrapper unwrapping/test, early typed eligibility/factory plumbing, compat-warmed runtime replacement, explicit admission/lock order, binding-change invalidation, no repeated deletion solely for missing system rows, exact active-row selection, synchronized metadata writers, no-op tool semantics, cache memory accounting and failure ordering.

Corrections to reviewer claims:

- The early classifier exists in `serve_handlers_responses.go`; absence of `uiSession` in the older monolithic handlers file does not mean eligibility is unavailable before factory creation.
- Tools-only changes correctly publish metadata rather than a false transcript edit. Provider-cache invalidation is an owned direct operation; no need to bump transcript_rev solely to signal it.
- Engine.ResetConversation does not erase the registered ToolRegistry or rt.systemPrompt, and there is no SetSystemPrompt method here. It does reset estimates/steering/pending-tool bookkeeping, so this plan avoids it and uses fresh candidate providers/existing provider-only reset instead.
- Missing stored prompt is a real supported shape, not proof that every web session lacks one (7835 explicitly had one). Its unknown-provider-history limitation is documented rather than solved with a new schema.
- Human-resumed branch conversations are not categorically excluded; background delegated execution is. Current first-party surface ownership determines eligibility.

The core policy remains unchanged: current system prompt + configured local tool list, automatically once per process/session, with existing explicit context changes respected. No historical-tool retention or provenance subsystem is added.
