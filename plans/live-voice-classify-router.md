# Live voice: classifier-backed control plane router

Status: **Phase 1 shipped on `feat/live-classify-router` (2026-09-19); Phase 2 remains open.** Builds on
`plans/live-voice-control-plane-isolation.md` Revision 3 (shipped). Nothing in that
design is superseded; this adds a second `live.Router` implementation and a config
selector for choosing between them.

Phase 1 (this PR) ships the classifier, the selector, the decision log, and the three
navigation/read labels. Phase 2 (separate PR, contracts below are open) adds
`steer_now` and `side`. The review that produced this split is summarised at the end.

## Problem

The shipped control lane (`live.control_plane: true`) triages every delegation with a
tool-calling fast-model turn (`serveLiveControlExecutor`). It works, and it is off by
default for a stated reason: a tool loop in front of every spoken request, work
included, is too slow and too expensive to leave on. So most calls run with no control
plane, and every "switch back to the other one" or "what's running" becomes a full chat
turn in whatever session the call is bound to.

A typed classifier (`term-llm classify`, TypeSafe System One via `internal/typesafe`,
already wired for `guardian.backend: classify`) answers the triage question in one
~650 ms request with a probability per label, no tool loop, no prose to parse. Only one
label — `switch_session` — needs a model that can look sessions up; the others map onto
host actions that exist.

Measured 2026-09-19 with `jev-latest` (`~/scratch/2026-09-19-intent-router/`): 30 real
user prompts from the session store, no session context, all of which should reach the
model: zero false `switch_session`/`new_session`/`cancel` above threshold. Navigation
and status examples classified ≥ 0.88 correctly with untuned criteria. Anecdotal — it
is the reason to keep the decision log, not a substitute for it.

## Taxonomy

| label | meaning | host action | phase |
| --- | --- | --- | --- |
| `steer` | ordinary request: a new turn if the bound session is quiet, guidance at the next safe boundary if a task is running | `RouteResult{Input: original}` — the controller/delegator already choose `Run` vs `Steer` (`serveLiveDelegator.Run`, `runSteeringDelegations`). The router never calls `Steer` itself | 1 |
| `status` | what is running / what happened | `sessionDirectory` (running + recent) plus jobs-v2 active/recent runs, rendered as one or two spoken sentences; `Handled` | 1 |
| `new_session` | start a fresh session and move the call to it | `liveNewSessionForTool` with empty project/agent (inherits the call's defaults); `Handled` with the spoken outcome | 1 |
| `switch_session` | move the call to a different existing session | switch resolver (below) | 1 |
| `steer_now` | same as `steer` but stop the running tool first | needs an owned interrupt-and-deliver operation; see Phase 2 | 2 |
| `side` | private question about the current conversation; no interference, no transcript trace | `startSideQuestion` on the bound session; see Phase 2 | 2 |

Deliberately absent: `prompt` (that is `steer` when quiet), `cancel` (Phase 2, as a
`steer_now` variant with an explicit stop-only result — **not** an empty `Input`,
which the controller reads as "use the original input"), and `backchannel` (the live
provider's VAD and turn detection own that).

In Phase 1 the classifier still asks about all six labels so the decision log records
`steer_now`/`side` frequencies; the gate maps both to `steer` unconditionally.

`steer` is the fail-open outcome of: classifier error, timeout, malformed answer,
unknown label, missing probabilities, and every below-gate label. The runtime worst
case is a wasted chat turn. (This claim is only true because Phase 1 has no action that
consumes the request without delivering it; Phase 2 must re-establish it per label.)

### Mixed requests

`also_request` (`noul`) detects utterances that navigate *and* ask for work ("in the
album chat, make the ornaments smaller"). A `noul` cannot extract the remainder, the
target project, or arguments. Phase 1 therefore does **not** attempt split delivery:
when `also_request ≥ 0.5` and the label is `switch_session`/`new_session`, the gate
returns `steer` with the whole utterance unchanged. The work is not lost; it runs in the
current session, which is today's behaviour. A structured navigate-then-continue result
is Phase 2 material and needs its own exactly-once delivery design.

## Classifier request

State is JSON (`--state-json` shape), not the bare utterance:

```json
{
  "message": "<delegation input>",
  "transcript_delta": "<RouteRequest.TranscriptDelta, bounded to N chars>",
  "active_run": true,
  "active_tool": "shell",
  "current_session_title": "…",
  "recent_session_titles": ["…"]
}
```

Questions (Go constants; the YAML under `~/scratch` is the dev harness):

- `intent` (`choice`, six labels). Criteria notes from the eval: the `steer`
  criterion must say explicitly that praise-plus-ask, comments inviting a reply, and
  instructions like "say ok" are `steer`; the `status` criterion must say general
  "is it working?" questions about a feature are `steer`. Both were observed misroutes
  with looser wording.
- `also_request` (`noul`), as above.
- `unambiguous_stop` (`noul`), recorded in Phase 1, used by `steer_now` in Phase 2.

Answer validation (all → `steer`, logged): missing `intent`, wrong answer type,
choice outside the six labels, missing/NaN/out-of-range probabilities, missing `noul`
values. The router uses a three-second classifier deadline when the provider has the
built-in default. Longer explicit provider timeouts are clamped to the remaining
controller budget (currently 13 seconds), preserving the resolver's 30 seconds and
a two-second margin inside the controller's 45-second backstop.

Note the input is voice-model-authored delegation text (`Router` doc in
`internal/live/controller.go`), not necessarily the user's verbatim words. Gates for
anything destructive (Phase 2) must be tested against that representation.

## Gates

Pure function `Gate(decision, cfg) → acted label`, table-tested.

| label | default gate | below gate | extra condition |
| --- | --- | --- | --- |
| `steer` | — | — | default |
| `status` | 0.60 | `steer` | — |
| `new_session` | 0.70 | `steer` | `also_request < 0.5` |
| `switch_session` | 0.70 | `steer` | `also_request < 0.5` |
| `steer_now` | (0.80, Phase 2) | `steer` | Phase 1: always `steer` |
| `side` | (0.85, Phase 2) | `steer` | Phase 1: always `steer` |

Utterances longer than 40 words skip the classifier and are `steer`. This is a
cost/latency exception, not a safety mechanism, and is documented as such.

There is no word-list tier. Revision 3's lesson stands: a hand-written rule is a word
list and a word list has a bottom.

## Switch resolver

Phase 1 ships one resolver: the existing `serveLiveControlExecutor`, built with
**registered tools, schemas, and allowlist restricted together** to
`session_directory` + `live_switch_session`, and a prompt written for switching only
(the general triage prompt tells the model to call `pass_to_workspace` and permits
new-session/voice operations; it is not reused). Engine construction in
`serveLiveControlExecutor` is factored around an explicit tool set so both the general
lane and the resolver share it.

The resolver returns a structured outcome; the router does not infer "switched" from a
generic `Handled`. Outcomes follow the shipped precedence rules exactly (tool evidence
wins; invalid args and `PERMISSION_DENIED` are not evidence; prose-only proves
nothing):

| resolver outcome | router result |
| --- | --- |
| `live_switch_session` succeeded | `Handled`, spoken confirmation naming the session |
| valid `live_switch_session` refused by the host (e.g. target hosts another call) | `Handled`, the refusal explained |
| valid `session_directory` call, no switch (ambiguous / no match) | `Handled`, spoken "which one?" naming the candidates |
| only invalid/unauthorised attempts, or prose-only | **fail open**: `steer` with the original input |
| provider error / timeout | fail open: `steer` |

The fourth row is the shipped policy, retained deliberately: a 0.70 intent probability
is not evidence that discarding workspace delivery is safe. The `search` resolver
(FTS + candidate `choice`) is cut from Phase 1.

## Decision log and authority

Three predicates in `LiveConfig`: router installed (`RouterEnabled`), mutating control
authority allowed (`ControlAuthorityAllowed`), host handling advertised
(`AdvertiseControlHandling`). With shadow mode removed they agree for every backend;
they stay separate so a future observe-only mode cannot accidentally install bindings.

Decision log: `route_decisions` table — live id, bound session, bounded state JSON,
probabilities, gated label, acted label, resolver outcome, latency, error. Surfaced by
`term-llm live decisions [--since]`. Privacy: this persists speech text and session
titles outside the transcript. `log_decisions: false` disables the table; `log_state:
false` (default true) keeps probabilities and labels but drops the state JSON. Rows
persist until the diagnostics `live.db` file is deleted; there is no automatic
retention or prune command. Document that "not in the transcript" does not mean
"not stored".

## Config

`live.control_plane` becomes a backend selector; the boolean keeps working.

```yaml
classify:
  default_provider: typesafe
  providers:
    typesafe:
      api_key: …            # or TYPESAFE_API_KEY
      model: jev-latest
      timeout_seconds: 3

live:
  control_plane: classify       # off | agent | classify   (bool accepted: true=agent, false=off)
  classify:
    provider: typesafe          # optional; defaults to classify.default_provider
    log_decisions: true
    log_state: true
    min_confidence:
      status: 0.60
      new_session: 0.70
      switch_session: 0.70
      steer_now: 0.80           # accepted and validated now; acted on in Phase 2
      side: 0.85
  control_provider: …           # still used by the switch resolver
  control_model: …
```

Selector migration, all of which is specified and tested:
- A normalised `LiveControlPlane` type with a decode hook accepting bool and string;
  accepted values `off|agent|classify` (plus `true`/`false`); unknown values are a
  config error; omitted/zero is `off`.
- `schema.go` default, `term-llm config get/set/show/reset`, completion (bool
  completion for this key is replaced by the three values), and `KeySpecs` for the new
  `live.classify.*` keys.
- The three existing gates (`startLiveController` router install,
  `withLiveControlAuthority`, `serve_live_context.go` advertised context) each switch
  on the appropriate predicate, not on a single truthiness check.
- `control_plane: classify` with no resolvable classify provider is a **configuration
  error** at startup and after a process reload (mirrors `guardian.backend: classify`). Runtime provider outages
  are fail-open. These are different and documented as such.
- `min_confidence` values must be finite and within [0,1] (mirrors
  `GuardianClassifyConfig.Validate`).

## Code shape

- `internal/live/classify/` (package name `liveclassify`), no server dependency:
  questions as constants, `State`, `Decision`, `Classifier` over `typesafe.Client`,
  `Gate`, answer validation. Fully unit-testable with a fake HTTP server.
- `cmd/serve_live_classify.go`: `serveLiveClassifyRouter` implementing `live.Router`;
  builds state from the record (bound session, active run/tool from
  `ensureResponseRuns()`, titles from the store), calls the classifier, gates, acts,
  logs. `status` and `new_session` handled inline; `switch_session` via the resolver.
- `cmd/serve_live_control.go`: factor engine construction around an explicit tool set;
  add the switch-only prompt and the structured resolver outcome.
- Client delegation mode (`serveLiveClientDelegator`): the classify router must not
  assume it owns the client's active work. Phase 1 has no action that touches a run,
  so this only matters for the `active_run` state field, which is best-effort.

## Docs (same PR)

- `docs-site/content/guides/live-voice.md` "The control lane": two backends; `agent`
  is the shipped lane and its text stays, scoped as such; `classify` is the one designed
  to stay on; the labels in user terms; fail-open contract; what Phase 1 does not do
  (stop, side questions, mixed requests, voice changes — the agent lane still handles
  voice via `live_settings`).
- `docs-site/content/reference/configuration.md`: `live.control_plane` row becomes a
  selector and its capability description is split per backend; new `live.classify.*`
  rows; cross-reference `classify.providers`.
- `README.md` only if it already mentions the control lane.
- This plan: Status updated when shipped.

## Tests

- `liveclassify`: gate table (every label × above/below × `also_request`), answer
  validation cases, state bounding, timeout.
- Router tests with a fake TypeSafe server: each Phase 1 label's action; timeout,
  malformed answer, unknown label → `steer` with original input; decision log row written
  and `log_state: false` honoured; `also_request` → whole
  request as `steer`; routing-queue overflow behaviour unchanged.
- Resolver outcome matrix (five rows above) against the restricted executor,
  including tool denial for `live_new_session`/`pass_to_workspace`.
- Config: bool→selector loading, unknown value rejection, omitted default, provider
  resolution failure at startup, `min_confidence` validation, completion values.
- Existing live control tests pass unchanged with `control_plane: agent` and with the
  legacy `true`.

## Phase 2 (not in this PR) — open contracts

- **`steer_now`.** `serveRuntime.Interrupt` takes text + fast provider and re-classifies
  with `auto` delivery; it is not a "cancel now" primitive, and unconditional rush
  delivery is explicitly excluded. Cancellation during a steering transition follows a
  separate rush-cancel path with response/run-epoch ownership checks (`serve_handlers.go`).
  The routing worker and delegation FIFO are separate queues, so "cancel, wait, return
  `Input`" is not atomic: queued work or a typed request can acquire the session between
  the cancel and the replacement guidance. Needs a transport-free host operation that
  pins session + expected run, distinguishes stop-only / cancel-then-replace / rush
  queued guidance, handles an in-flight transition, and admits the replacement exactly
  once — preferably by extracting the steering-transition ownership machinery rather
  than adding a third cancel path.
- **`side`.** Feasible: the side engine has independent state, snapshot, cancellation
  and concurrency tests. Open: ownership when the browser already has an active side
  question; how the router obtains its own terminal answer; shared vs separate history;
  behaviour on call close/rebind; and the fact that a slow side answer occupies the
  serial routing worker (the 45 s backstop is cooperative). A request accepted as
  private must not fall through to the transcript on failure — it needs a handled
  failure answer.
- **Mixed requests** with structured extraction and exactly-once continuation.
- **`search` resolver**, once the decision log shows what people actually say.

## Review record

Design review (gpt-6-astra, 2026-09-19) found four blocking issues in the first draft:
`steer_now` mapped to a primitive that does not exist in that form and raced the
delegation FIFO; `also_request` cannot extract a remainder; the switch resolver
contradicted the shipped fail-open precedence; a shadow mode was described as
equivalent to off. All four are addressed above by deferral (`steer_now`, `side`, mixed
delivery, `search`) or by adopting the shipped policy (resolver outcomes). Shadow mode
was built and then removed before merge: Sam was not going to use it, and the three
authority predicates only had to differ because of it.
