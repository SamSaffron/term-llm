# Explicit script exposure and safe tool composition

Status: implemented

Implemented and group-reviewed on 2026-11-14. All runtime phases below are shipped
in this worktree; no live server restart or real-model invocation was performed.
See the implementation/verification record at the end for deliberate boundaries
and the remaining repository-wide checks failing outside this feature.
This plan covers every shipped builtin, non-builtin engine tool, incoming MCP tool,
client/WebMCP tool, and outgoing MCP surface. Availability remains mode/config dependent; script eligibility never
enables a tool, supplies credentials, or grants permission.

## Objectives and decisions

- Make `spawn_script` useful for parallel agent orchestration without direct-call
  workarounds, while preserving permission, ownership, cancellation and audit rules.
- Separate permission classification (`ToolKind`) from script exposure. Neither
  `KindAgent` nor `KindSessionState` is intrinsically a reason to exclude a tool.
- Use explicit per-tool exposure with one effective policy path for admission,
  descriptions, skill-module dependencies and dispatch. Undeclared new tools fail
  closed for script exposure, without disappearing from direct calls.
- Do not copy Codex's entire exposure framework: term-llm already has registration,
  deferred provider schemas, allowlists and mode gating. Add only the independent
  script-eligibility dimension, with a stable direct-only reason.
- Keep immutable cell capability ceilings, live narrowing/revocation, exact tool
  identity, output bounds, approvals, concurrency limits and finishing invariants.
- Do not widen public `serve mcp` exports. Support client/WebMCP composition only
  while its originating foreground response/client binding is valid. Background
  client access, reconnect/rebinding and durable client capability leases are out
  of scope; server-owned capabilities may still outlive a response.

## Pre-implementation findings and concrete hazards

`internal/tools/descriptors.go:codeModeDirectOnlyTools` excludes whole permission
categories. `internal/llm/engine/codemode.go:directOnly` repeats a hardcoded name list.
The latter says `ui_activate`, while the real tool is `ui_activate_extensions`.
Admission, identity checks and module dependency validation are separate consumers.

`captureCodeModeAuthority` in `internal/tools/codemode_authority.go` reconstructs
local builtin tools against snapshot permissions. Only a few runtime adapters are
preserved. Newly admitting agents by deleting names would lose configured runner,
child-registry and runtime bindings; agent control construction is order-sensitive.
Other tool families currently use their live instance instead of this shadow.

`nestedCell` snapshots `executionSnapshot`, including deferred execution entries,
not just provider-visible schemas. Incoming MCP identity is checked against live
catalogue hashes and server policy by `internal/mcp/tool.go`. Registration alone
is not permission and discovery alone is not a grant.

Several apparent state tools do not commit their effect in `Execute`: goal and
progress host trackers observe tool calls/results. `finalize_progress` is a genuine
`FinishingTool`; it must remain direct-only. This corrects the earlier suggestion
that all progress tools could simply be exposed.

## Complete exposure inventory

Legend: **Allow** is the target once binding/tests below pass; **Direct** stays
outside scripts; **Deferred** means a reviewed direct-only decision for this release, with future work stated. No Allow row
is shipped before its authority and effect semantics are verified.

### Descriptor-backed tools (`internal/tools/descriptors.go`)

| Tool(s) | Target | Binding/reason |
|---|---|---|
| `code_mode` | Direct | Legacy synchronous script control; preserve compatibility |
| `spawn_script`, `wait_script`, `cancel_script` | Direct | No recursive cell lifecycle control |
| `read_file`, `write_file`, `edit_file`, `unified_diff` | Allow | Snapshot file permissions/base directory/recorders |
| `shell` | Allow | Snapshot execution policy, approvals, collaborative shell adapter |
| `grep`, `glob` | Allow | Snapshot search permissions and reranker |
| `view_image` | Allow | Preserve vision adapter and snapshot read permissions |
| `show_media`, legacy `show_image` | Allow | Preserve publisher, path approval and media bounds |
| `image_generate` | Allow | Preserve image provider, approval and publisher bindings |
| `ask_user` | Direct | Explicit foreground interaction; normal tool approvals still supported in cells |
| `spawn_agent` | Allow | Shared configured runner/registry, immutable parent policy/context |
| `wait_agent`, `continue_agent`, `cancel_agent`, `list_agents` | Allow | Bind same child registry; preserve ownership, depth/model and concurrency policy |
| `queue_agent`, `wait_for_jobs` | Allow | Preserve jobs client, trusted origin, pinned run identity and progress callbacks |
| `run_agent_script` | Allow | Fixed admitted agent source/config snapshot; filename-only trusted bundled scripts; no arbitrary path grant |
| `initiate_handover` | Direct | Transfers runtime/instruction ownership |
| `hub_delegate`, `hub_check_delegation` | Deferred / Direct | Credentialed cross-host delegation needs independent effect-reconciliation and ownership tests; no exposure expansion in this release |
| `manage_workspace` | Direct | Alters authority; cannot enlarge a running cell's ceiling |
| `update_plan` | Allow | Share session-keyed controller, not a fresh empty controller |
| `live_settings` | Direct | Changes runtime/configuration under active execution |
| `ui_get_source` | Allow | Snapshot read policy and source directory |
| `ui_extensions` | Direct | Mixed status/reload API; keep whole tool direct rather than add argument-sensitive exposure |
| `ui_activate_extensions` | Direct | Process/browser extension lifecycle; use real registered name |
| `activate_skill` | Direct | Changes instructions, registered tools and admitted modules; new cells may use activated modules |
| `search_skills` | Allow | Snapshot/bind skill registry, search must not implicitly activate |
| `live_new_session`, `live_switch_session` | Direct | Changes session/runtime ownership |
| `session_directory` | Direct | Only on the throwaway live-control engine; no reason to create a new cell-owned directory service |
| `create_goal`, `update_goal` | Deferred / Direct | Host trackers commit direct events; a nested call would return success without persistence. Scoped commit services are separate future work |
| `get_goal` | Allow | Read bound originating goal; no fallback to another session's current goal |
| `update_progress` | Deferred / Direct | Event-only persistence/tracking is not a composable service; reject nested calls, including `final: true` |
| `finalize_progress` | Direct | Finishing tool regardless of parameters |

### Tools outside the descriptor registry

| Tool/family | Target | Owner and constraints |
|---|---|---|
| `web_search`, `read_url` | Allow | `internal/llm/*_tool.go`, `internal/app/runtime/engine.go`; preserve provider/network policy and output limits |
| `tool_search` | Direct | Per-run planner exception, discovery cutoff and schema-activation state do not safely outlive a run. Scripts already have authorized deferred tools and `describeTools` |
| `suggest_commands` | Direct | Structured-output passthrough, not an executable ordinary registered tool |
| Configurably named `set_output` tools | Direct | `internal/tools/set_output.go`; all finishing tools invariant, independent of name |
| Agent custom script tools | Allow | `internal/tools/custom_tool.go`; snapshot admitted source/config/approvals, preserve output handling and identity |
| Skill script tools | Allow | Same, plus project trust gate and activation generation; no implicit activation |
| `memory_search_fragments`, `memory_list_fragments`, `memory_get_fragment` | Direct | Dedicated mining engine only; preserve collector/budget scope, do not add script support without a use case |
| `memory_create_fragment`, `memory_update_fragment` | Direct | Dedicated mining engine writes carry collector/dry-run state; no new nested mutation surface |
| `pass_to_workspace` | Direct | `internal/app/serve/serve_live_control.go`; finishing handoff |
| Client-supplied `client_tools`, page/WebMCP `webmcp__*` | Foreground-only Allow | Explicitly admitted from the originating request and a host-owned runner lease; fail closed when that response/client binding ends. No background/reconnect support |
| Incoming MCP tools (arbitrary names) | Allow | Authorized exact catalogue identities only; full contract below |
| Future/unknown engine tools | Direct by default | Require explicit script policy and binding review before admitting |

Inventory tests must enumerate the descriptor table and registration families
above. Alias names inherit canonical policy. Do not rely on a grep-based assertion
that every Go `Spec()` method was found: combine registration-path tests with an
explicit expected classification list and source audit for nonlocal families.

## Policy architecture

1. Introduce a small shared exposure contract in `internal/llm/core`, implementable
   by tool wrappers without package cycles. It returns eligibility and a stable
   exclusion reason; zero/unspecified fails closed for nested calls. Builtin
   descriptors explicitly declare policy and registry wrappers carry it to engines.
2. Tool instance declarations cover non-descriptor tools, custom/skill scripts,
   MCP and host adapters. Preserve optional identity/finishing interfaces through
   wrappers. Finishing/script-control invariants cannot be overridden by metadata.
3. One engine resolver computes effective eligibility from registered identity,
   declared exposure, binding availability and live permission policy. Use it at
   admission, `Describe`, dependency validation, before/after dispatch permit waits,
   and dispatch. Exposure belongs to the registered instance: replacement by a
   Direct-only instance narrows existing cells even if its schema is unchanged;
   widening affects only newly admitted cells. Client tools additionally require a
   live foreground lease and never fall back to registered/MCP name lookup. Track binding/origin replacement
   independently of schema equality where required.
4. Eliminate `SetCodeModeDirectOnly` category inference and the ordinary builtin
   string blacklist after all constructors/registration paths declare metadata.
   Preserve legacy `code_mode` behavior through the same resolver.
5. Avoid a new user-config exposure language in the first release. Existing
   enable/deny/MCP server policies remain authoritative. Future narrowing-only
   configuration can be added separately without creating an alternate grant path.

## Authority, context and runtime ownership

- Use explicit binding adapters for snapshot-based local effects versus services
  that must share a scoped registry/controller. Do not reconstruct a configured
  service through a generic constructor or shallow-copy mutable locks.
- Bind agent controls in two passes: establish the configured shared child service,
  then bind controls to it. Never depend on map iteration order or create a second
  child registry. Do not close the shared registry when one cell ends.
- Capture originating session/user/workspace, agent/depth, model policy, parent
  child identity, goal scope, jobs notification origin and host wake
  callback at admission. A later turn/session must not rebind these to itself.
- Audit request-only callbacks in `serve_run_context.go`, TUI/shared runners and
  `codeModeSession.admit`: background cells must use cell-owned routing, not retain
  a retired foreground sender or unguarded browser/user identity.
- Preserve the existing frozen ceiling intersected with the live filter. A cell
  admitted under request `AllowedTools` stays narrow after the filter is restored.
  A later narrowed request may transiently deny an older cell; report that as
  `permission_denied` / `not_dispatched`, not an unknown effect. Do not add filter
  widening or isolate older cells from live revocation in this change.
- Never capture the planner-only per-run `tool_search` exception into a cell ceiling.
  Ordinary nested tools remain subject to their admitted and live allowlists.
- Missing required binding fails admission or the affected tool's exposure with a
  stable error; never silently fall back to a mutable live tool for scoped effects.
- Custom/skill scripts preserve source directory, permission scope and generation;
  removal/replacement invalidates old cells even when the input schema is equal.
- For goal/progress mutations, do not synthesize nested events into direct-only
  trackers as a quick fix. Keep direct-only pending a scoped commit service with
  exactly-once observable persistence and consistent direct/nested results.

## Agent/job cancellation and effect accounting

- A waiting tool call respects cancellation. Cancelling a cell is not equivalent
  to cancelling every independently started child/job/delegation.
- Started durable work retains its existing owner/session semantics; explicit
  `cancel_agent` stops a child. Human Stop/session shutdown retains current broader
  teardown behavior; this plan does not weaken it.
- Before acknowledgement, a cancelled spawn/queue/delegate may have an unknown
  outcome. Preserve actual-effect leases/quarantine and no-auto-replay rules.
- Correlate nested call ID, cell ID and child/job/delegation ID in bounded receipts
  and host audit so work can be reconciled even when text was not emitted or the
  result overflowed. Receipt retention obeys existing bounded lifecycle rules;
  do not promise process-restart durability where none exists.
- Preserve engine agent depth/concurrency/model restrictions as well as code-mode
  bridge call/parallelism limits. Test nested long waits cannot starve unrelated
  allowed calls; park permits only at existing safe approval/wait boundaries.

## Incoming MCP contract

- Allow already-authorized incoming MCP tools, including deferred execution tools,
  as today. Provider schema visibility/activation is a presentation budget, not a
  permission boundary. Do not require planner-active status just to compose calls.
- Keep `tool_search` direct-only: it mutates per-run schema activation and its
  special permission expires with the originating run/cutoff. Scripts inspect
  admitted schemas through `describeTools`. Newly registered tools require a new
  cell; selecting provider schemas never expands an old ceiling.
- `tool_exec.go` already calls `Planner.ToolExecuted` for nested calls. Preserve
  that hook, do not duplicate it. Test attribution to the originating session after
  the run retires, and preserve active-tool/LRU semantics without inventing grants.
- Identify tools by trusted registration provenance plus exact server/original name,
  not local-name matching, delimiter splitting or server-supplied annotations.
  A remote tool named `spawn_agent` must not inherit local agent service privileges.
- Preserve server denial, OAuth/credentials, live catalogue identity/schema checks,
  removal, reconnect and output/error semantics. Test swapping server configuration
  under the same name/schema: add a binding generation only if existing exact
  identity cannot reject a changed authority target. Ordinary reconnect to the
  same trusted target must not be needlessly invalidated.
- MCP read-only/destructive/idempotent hints are untrusted metadata, never permission
  or exposure grants. Do not invent a new MCP approval regime in this change;
  preserve current direct-call authorization and clearly document that scripts can
  fan out remote effects (currently 64 calls per cell, four concurrent per cell,
  eight live cells per session, plus session-wide limits). Any approval expansion
  is a separate security project; do not multiply these into a new promised limit.
- Prevent exposure bypass through name collisions/overwrites. Establish explicit
  origin identity and reject cross-origin replacement of an occupied registered
  execution name. Define origin classes (builtin, custom agent, skill, MCP server,
  planner, host-scoped) and explicit scoped override/restore rules. Preserve valid
  skill-over-custom replacement/restoration, same-origin catalogue refresh, goal
  re-registration and configured builtin-adapter replacement. Old cells must not
  silently follow a replacement into a different authority scope.
  Audit legacy single-server names, sanitized-name collisions, builtin/custom/skill
  conflicts and reserved client `webmcp__` routing. Make compatibility errors clear;
  do not silently rename existing public tool calls.
- A remote MCP tool named `spawn_agent` receives no local agent binding or special
  privilege. Test this explicitly. Do not infer remote semantics from names or add
  speculative loopback-name blacklists; trusted provenance determines local policy.

## Outgoing MCP and client boundaries

- `internal/app/serve/serve_mcp_host.go` exports a curated file/shell/search/image/web
  set. Keep that list unchanged. Its executor calls tools directly without a script
  controller; exposing `spawn_script` there would require a separate owning runtime.
- CLI-provider loopback MCP (`internal/mcphttp`, `internal/llm/providers/clibin`) mirrors request
  tool specs. Preserve exposure metadata/provenance across projection where needed,
  but do not remove legitimate direct-only tools from its direct-call surface.
  Phase 1 must trace every `SetToolExecutor` caller (including RetryProvider):
  either establish that its raw executor lane is unreachable in live turns, or
  route reachable execution through equivalent allowlist, controller and audit
  checks. Do not apply script-only restrictions to legitimate direct tool calls.
  Provider parity tests gate any newly enabled nested agent calls.
- Deprecated `mcp-server --tools-json` is an echo compatibility surface; no new
  executable capabilities or exposure claims.
- Client/WebMCP tools use the foreground-only capability contract below. Do not
  register request-supplied tools globally or let a registered/MCP name acquire
  client routing. Unsupported client transports remain direct-only/passthrough;
  a schema alone is insufficient to promise inline execution from a script.

### Foreground-only client composition

This is an explicitly narrower addition than background browser/client tooling.
A script cell may survive its response; its client capabilities may not.

- At admission, capture only tools explicitly offered by the originating request
  and owned by that request's scoped `ClientToolRunner`. A request-declared tool
  with no inline runner remains unavailable inside scripts. Generic passthrough
  providers that require ending a turn and a follow-up client request are not made
  inline-capable by this plan.
- Capture full offered specs/hashes, origin response ID, session/user/client binding
  and a host-owned revocable foreground lease. `ClientToolRunner` currently exposes
  names and execution, not complete schemas or a lifetime signal: add an explicit
  host contract rather than retaining request context and assuming it stays valid.
- Merge eligible client entries into the cell's immutable catalogue through a
  separate provenance-bearing resolver. Do not put them into global engine tools.
  Capture only the scoped runner's offered subset; child agents cannot inherit a
  parent's client capabilities simply through context inheritance.
- Preserve the existing client authorization model: the client decides execution
  of its offered tools. Do not accidentally grant local file/shell permissions or
  reinterpret client offer as server-tool authorization. Cross-origin name conflicts
  must fail clearly; never dispatch to another origin as a fallback.
- Check the lease at admission and immediately before dispatch, including after any
  permit wait. Use atomic lease dispatch admission to resolve completion/disconnect
  races. The lease expires irreversibly on originating response completion/failure/
  cancellation, relevant client disconnect/navigation or host session/client-owner
  change. Define concrete hooks for typed response and live runners separately.
  If a transport cannot signal validity loss reliably, do not expose its tools yet.
- Expiry does not cancel the whole cell or its server-owned tools. Later client
  calls fail with a stable `client_binding_expired`-style error and
  `outcome: not_dispatched`. `wait_script`, a new response, reconnection or a new
  same-named page tool cannot renew/rebind that cell's client capability.
- `spawn_script` max_wait/observation timeout alone is not lease expiry: the
  originating response may still be active. Conversely, a response finishing
  invalidates the lease even if its script is still live. Do not extend a response
  indefinitely just to keep an orphaned script's client tools available.
- In-flight client calls end on lease expiry, cell cancellation or existing client
  timeout, whichever comes first. If a request could already have reached the
  device, preserve `outcome_unknown` rather than claim it was not dispatched.
  Fence late/duplicate answers by response, client generation and call ID; never
  deliver them to another cell/response or automatically replay a device action.
  Keep bounded pending-call/tombstone state so actual-effect accounting cannot be
  stranded indefinitely by a client that never replies.
- Route through the same bounded nested call/argument/result and audit machinery.
  Preserve existing client limits, result envelopes, explicit media emission and
  no-auto-replay rules. Inline client waits must not monopolize permits needed to
  deliver results or process cancellation; use existing safe wait/pause patterns.
- Foreground request/result delivery uses the existing typed/live response channels,
  not a new background notification protocol. The implementation maps each binding
  to explicit close/cancel hooks and proves result handling remains routable while
  the lease is active. It never adds a global `ClientTool` fallback to nested calls.
- Initial coverage: mock client runner plus typed-response and live inline runner
  integrations where the host can provide these lifetime guarantees. Unsupported
  transport/provider combinations fail closed and retain normal direct passthrough.
  No durable browser capability, automatic reconnection or cross-response continuation.


## Discoverability and documentation

- `Object.keys(tools)` remains the exact admitted callable catalogue; no fake
  callable entries for excluded tools. `describeTools` retains its bounded schema
  contract. Explain that directly callable tools may be absent from scripts.
- Publish the complete exposure matrix and reasons in developer/user docs. Defer
  a new runtime exclusion-query helper: it is not needed for the orchestration fix
  and would add an unauthorized-name disclosure surface.
- Document frozen admission, authorized deferred MCP access, skill activation/new
  cells, foreground-only client/WebMCP expiry, native/MCP result envelopes and explicit child
  cancellation. Update generated prompt sources, `internal/codemode/README.md`
  and `docs-site/content/` references.

## Implementation sequence and acceptance gates

1. **Policy foundation, provenance and bridge audit.** Add metadata/resolver and
   exhaustive classifications. Preserve the current admitted set for an ordinary
   fixture registry until intentional deltas land; classify test doubles explicitly.
   Fix collision/override policy and trace the CLI-provider raw executor lane now,
   before expanded exposure. Delete duplicate filters after parity tests pass.
   Dedicated mining/live-control engines are intentional narrowing exceptions;
   record them rather than claiming byte-identical global parity.
2. **Safe ordinary composition.** Preserve existing user-facing allowed tools with
   explicit bindings; add `search_skills`, `update_plan`, `run_agent_script`.
   `run_agent_script` has a concrete use case: compose an already-authorized bundled
   build/test helper with tool results in one script; it must retain filename-only
   source confinement and its existing trust model, not inherit arbitrary execution.
   Verify actual plan-controller persistence/UI observation, custom/skill ownership,
   scoped `get_goal` removal, and all live permission/identity checks.
3. **Agent lifecycle composition.** Bind the existing configured child service and
   controls, not newly constructed runner-less tools. Snapshot parent policy/context
   separately from shared service state. Add spawn/wait/continue/cancel/list together.
   Verify `tools/agentwait` accepts nested wait evidence for turn finalization and
   cell-owned callback/event routing survives the originating request.
4. **Durable jobs composition.** Bind `queue_agent` and `wait_for_jobs`, trusted
   notification origin and pinned run identity; test unknown outcomes, child/job
   ownership and shutdown. Hub delegation remains Direct pending separate work.
5. **Foreground client composition.** Add explicit host-owned lease/spec capture
   for scoped inline client runners, per-cell client resolution, atomic dispatch
   and expiry/result fencing. Wire typed/live lifecycle hooks and test unsupported
   providers fail closed. Do not add global request-tool registration or background
   delivery. This phase must pass its own safety gates before enabling client tools.
6. **Documentation and full verification.** Publish the matrix and migration notes,
   verify outgoing MCP exports remain unchanged, run all implementation checks.
   Goal/progress mutation services, hub expansion, durable/background client cells
   and a new diagnostic API are explicitly outside this implementation.

Tests use mock providers, fake runners/clients and local MCP fixtures, never live
model calls/API keys or the user's HOME. Required coverage:

- Every builtin/alias and nonlocal family has explicit policy; unclassified defaults
  to Direct. Direct denied tools stay denied; direct-only tools remain directly usable.
- Allowed-to-Direct policy change, permission revoke, removal and same-schema
  replacement invalidate old cells; widening does not expand old cells.
- Original session/request allowlist, depth/model/workspace and notification origin
  remain pinned across request timeout, turn completion, foreground/session switch.
- Agent spawn/list/wait/continue/cancel share one service; parallel spawn, terminal
  failure, admission failure, partial failure and explicit cancellation are observable.
- Regression scenario: a script concurrently spawns several mock model-specific
  workers and collects native envelopes without a direct-call fallback. Verify
  nested attribution in an engine harness and one serve integration test; no real
  model dependency. Other adapters retain their existing event projections.
- Cell cancellation before/after dispatch, unknown spawn/queue outcomes, oversized
  results, child survival and session shutdown obey defined ownership/lease rules.
- Agent control binding is independent of sorted-name construction (cancel precedes
  spawn today), and nested waits satisfy existing agentwait completion bookkeeping.
- `tool_search` is absent from the script catalogue; retired-run calls do not gain
  its planner exception. Existing MCP usage is attributed without duplicate hooks.
- Nested goal/progress calls are rejected and host trackers ignore parent-attributed
  nested events rather than committing unrelated direct state.
- Scoped `get_goal` is removed on goal-loop exit; surviving cells get a clear denial.
- Plan state changes persist through the actual controller; no no-op success for
  newly allowed tools. Goal/progress/finishing/handover calls remain excluded.
- MCP deferred authorised calls, discovery no-grant, policy denial, schema/removal/
  generation changes, annotations, origin/name collisions and malicious control-like
  names. Preserve structured errors/content and no replay after completed effects.
- CLI-provider loopback direct-only calls remain available; scripts cannot recurse
  or bypass allowlists through aliases. Client/WebMCP tools require explicit
  offered schemas plus a live scoped inline runner, never ambient context alone.
- Foreground client calls return to the originating script through mock, typed
  response and live inline runners. Offered schema descriptions are accurate;
  unoffered tools and passthrough-only tools are absent from the callable catalogue.
- Response end, disconnect/navigation, owner switch and client generation changes
  revoke only client access. Test dispatch/expiry races, permit-wait expiry, cell
  cancellation, client timeout, late/duplicate replies and independent server calls.
- After expiry, undispatched calls report `not_dispatched`; dispatched unacknowledged
  device effects report `outcome_unknown`. Reconnection/new response/wait_script do
  not resurrect capability or replay actions. No pending-call/permit leaks.
- Observation timeout while the response remains active does not expire client
  access. Response completion while the cell survives does. Child agents cannot
  inherit client capability; client/MCP/builtin collisions cannot change routing.
- Parity for legacy code_mode, async cells and skill-module dependency admission.
- Race tests for shared agent registry, policy/catalogue replacement and cell teardown.

Implementation verification commands (adapt focused package list to touched files):

```sh
gofmt -w <changed-go-files>
make test PKG='./internal/tools ./internal/llm/... ./internal/codemode/... ./internal/mcp ./internal/tooldiscovery ./internal/mcphttp ./internal/app/runtime/... ./internal/app/serve'
make test PKG='./internal/tools ./internal/llm/engine ./internal/mcp ./internal/app/serve' ARGS='-race'
make docs
make build
make check-changed
make check-changed ARGS=--all
make plans-lint
make complexity
```

No baseline increases to hide growth; split oversized implementation files. Nested
terminal/frontend checks apply only if implementation actually touches those areas.
For this planning-only change, run plans lint, changed-area checker and complexity;
record broad implementation checks as pending rather than claiming they ran.

## Refinement record

Reviewed and refined by `claude-bin:fable-medium` on 2026-11-14. Incorporated:
per-run `tool_search` exclusion; existing planner usage hook; direct-only dedicated
memory/live-control tools; explicit goal/progress no-op hazard; hub deferral; early
collision/bridge gates; valid override/restore cases; scoped goal lifetime; test-double
classification; corrected provider package paths; no new diagnostic API.

Retained `run_agent_script` eligibility with an explicit bundled-helper composition
use case and source/trust binding tests; this is not permission to run arbitrary
scripts. Kept runtime binding design service-oriented rather than mandating reuse
of an entire mutable tool instance: share the configured child registry/runner,
while pinning per-cell policy and parent context. Fable identified no intrinsic
reason agent orchestration cannot be supported once those gates pass.

Subsequent user-approved scope refinement: add foreground-only client/WebMCP
composition with a revocable originating-response lease. This replaces the initial
blanket client exclusion, not the exclusion of durable/background client access.
Added a dedicated implementation phase and expiry/dispatch/result-fencing tests.
The initial Fable review preceded this addition; it is not represented as a review
of the later client lease design.

Planning verification: `make plans-lint`, `make check-changed` and `make complexity`
passed. Complexity reports pre-existing warnings/baseline shrink notes; no ledger
changes. Builds, Go/race tests and the full suite are implementation gates, not run
for this Markdown-only change. This document records the approved design and implementation boundaries.


## Implementation and final review record

Shipped policy and bindings:

- Explicit `core.CodeModeExposureProvider` and descriptor-owned builtin policy;
  unclassified tools fail closed for scripts. `CodeModeLifecycleTool` and
  `FinishingTool` remain invariant exclusions. `SetCodeModeDirectOnly` is retained
  only as a deprecated, narrowing-only compatibility setter (not used for builtin
  classification).
- Exact engine/server instance, generation, live binding and schema identity are
  pinned. Both legacy and async admission capture local authority. The capture
  callback receives the admitted server instance map and may bind only instances
  the local registry actually owns. Missing services narrow the pre-VM catalogue
  instead of disabling unrelated tools or advertising callable denial stubs.
- Agent lifecycle tools share the configured child manager while freezing policy;
  job clients/origins, actual plan controller, skill search and bundled script
  sources are bound. Child engine runs clear inherited cell/controller/pause and
  private execution override/lease keys. Passive agent/job waits park shared
  execution permits without changing direct-call partial-result semantics.
- Incoming MCP keeps exact trusted origin/target/credential identity and stable
  identical refreshes. Discovery surfaces only installed intended bindings; rejected
  registrations cannot be advertised, indexed or later delete another tool.
  Conditional cleanup is atomic and accepts idempotent same-binding refreshes.
  Serve cleanup also uses actual server provenance, including sanitized names,
  rather than deleting arbitrary local tools by a name prefix.
- Foreground client capabilities are request-scoped, never globally registered.
  Original typed POST streams carry an opaque delivery marker; concurrent/resumed
  streams and generic recovery snapshots cannot receive foreground-only actions.
  Live client leases revoke on subscriber, owner, generation and response changes.
  Publication, expiry, pending/duplicate answers and late results are fenced;
  device actions are never automatically replayed. Public MCP exports are unchanged.
- Source-backed tool reference now includes script exposure and exclusion reasons.
  Shell/Git inspection, argv/leading-directory parsing and command preparation
  helpers were mechanically extracted into their owning utility packages to meet
  package-size limits; existing behavior/tests are preserved and no ledger changed.

Deliberate boundaries/refinements:

- Custom/skill/bundled script source DIRECTORY, definition and permission scope are
  pinned, not executable file bytes. Contents remain live as with direct filesystem
  and shell execution; this permits creating/editing a helper in the same cell.
- The typed client identity is an opaque original-stream identity, not a stable
  browser identity; background/reconnect/rebinding remains unsupported. Each lease
  has a documented fixed 128-call anti-replay budget across its cells.
- Goal/progress mutations, hub expansion, user interaction, authority/session/skill
  activation, finishing controls and planner exceptions remain direct-only as
  specified. `get_goal` retains its original nonlocal closure and loses access on
  scope exit or replacement; regression coverage proves no cross-session fallback.

Concurrent group review used Gemini (`agy-bin:gemini-4-hydrogen-xhigh`), Grok
(`cursor-bin:grok-4.7-high`), Opus (`claude-bin:opus-max`) and Muse
(`opencode-go:muse-spark-1.3-contributor-xhigh`). Actionable findings were fixed and
rechecked: local/engine authority divergence; rejected discovery ownership and
cleanup; concurrent typed resume routing; expired result/receipt acceptance;
publication certainty; sanitized client namespace collisions; repeated override
restoration and comparable-interface safety. Final reviews found no remaining
production blockers. Source-byte freezing and a suggested `get_goal` rewrite were
withdrawn after confirming the intended contract and the actual scoped mechanism.

Verification executed:

- `make docs`, `make build`, root build/vet, plan lint and complexity passed.
- Complete affected suites passed: tools, core/engine, codemode, MCP/discovery,
  skills, serve, docs generation and extracted filetrack/procutil/shellcmd helpers.
- Full affected race runs passed except a legacy polling fixture's unawaited
  background callback; fixed the fixture to release and await terminal completion.
  Full engine `-race -count=2` then passed, alongside full race suites for core,
  tools, MCP, discovery and skills and focused serve client/MCP lifecycle races.
- `make check-changed ARGS=--all` was run twice: build/vet, nested modules,
  frontend formatting/typecheck/tests, complexity and plan lint passed. The
  feature-related fixture and MCP cleanup failures found during those runs were
  corrected and retested. Remaining broad-check failures are unchanged frontend
  `toggle-switch.css` comment spacing, the `app.css` gzip budget (35,254 bytes vs
  35,250 allowed), and `TestAgyTerminalFailureEndsTurn/error_result_cancels_process`
  taking about 30s rather than under 5s in unchanged provider production code.
  No config/cache/credential/generated-budget changes were made to hide failures.

No commit was requested or created. The archived status records implemented
runtime behavior, not a claim that every repository-wide check is green.
