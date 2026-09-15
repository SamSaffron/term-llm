# Live voice mode (gpt-live-1-codex) for the web UI

Status: implemented, with prompt/context and tool-plumbing follow-up complete.
See `docs/live-voice.md` for current behavior and configuration. The original
design below includes historical implementation sketches; Codex citations refer
to the `../codex` Rust checkout (`codex-rs/...` paths).

## Goal

1. A "Live" toggle in the web composer. When on, the send button becomes a
   **Live** button. Clicking it opens a bidirectional voice conversation with
   OpenAI's `gpt-live-1-codex` model, bound to the current chat session.
2. The voice model owns the conversation. When it needs real work it emits a
   `delegation.created`; term-llm runs that request as a normal turn in the
   bound session — same engine, same tools (including `spawn_agent`), same
   approvals and transcript persistence — and streams the result back so the
   voice model can speak it.
3. First implementation ships one provider, `chatgpt` (the OAuth credentials
   term-llm already stores). Config and the `live.Provider` seam follow the
   `image`/`audio`/`transcription` pattern so `openai` (API key) and
   `elevenlabs` can be added later without touching serve or the frontend.
4. `internal/live` (protocol client + controller + delegation contract) is
   UI-agnostic so a later TUI host can reuse it unchanged (see "Follow-up:
   TUI").

Explicit non-goals for v1: TUI support, text-only live sessions, persisting
spoken-only exchanges into the transcript, multiple concurrent live sessions
per chat session, and Hub proxying of live audio.

## What the Codex source actually does (verified)

The summary in the task description mixes two layers. `thread/realtime/start`
with `version: "v3"` / `clientManagedHandoffs` is the **Codex app-server
JSON-RPC** API (`codex-rs/app-server-protocol/src/protocol/v2/realtime.rs:197`).
Underneath, Codex core talks to OpenAI with two plain connections. term-llm
does not need an app-server or a Codex thread; it needs to reproduce the layer
below.

### Call creation (HTTP)

- `POST https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`
  — the `/backend-api` provider branch selects path `realtime/calls` and the
  AVAS query params (`codex-rs/codex-api/src/endpoint/realtime_call.rs:62-79,213-224`;
  asserted at `:696-727`). Public-API providers use `POST {base}/v1/live`
  multipart instead (`:168-204`) — that is the path that 403s with an OAuth
  bearer; we do not use it.
- Body is JSON `{"sdp": "<offer>", "session": {...}}`
  (`realtime_call.rs:43-47,147-162`).
- `session` for the frameless/V3 protocol
  (`codex-rs/codex-api/src/endpoint/realtime_websocket/methods_frameless_bidi.rs:52-99`):

  ```json
  {
    "model": "gpt-live-1-codex",
    "instructions": "...",
    "audio": { "output": { "voice": "cove" } },
    "delegation": { "type": "client", "ack_filler": true },
    "initial_items": [
      {"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]},
      {"type":"message","role":"assistant","content":[{"type":"output_text","text":"..."}]}
    ]
  }
  ```

  `id` is stripped before send (`realtime_call.rs:142-145`). `initial_items`
  and `ack_filler` are optional. Roles `user`/`developer` → `input_text`,
  `assistant` → `output_text`. Voice values are the snake_case
  `RealtimeVoice` enum (`codex-rs/protocol/src/protocol.rs:283`).
- Auth headers are the ordinary ChatGPT ones: `Authorization: Bearer <oauth
  access token>`, `ChatGPT-Account-ID`, `originator`, `User-Agent`
  (`codex-rs/core/src/client.rs:412-421,593-621`). Attestation
  (`x-oai-attestation`) is only added when the provider supports it
  (`client.rs:454,742-754`). Client source alone does not establish backend
  attestation policy; direct OAuth calls with `cove` have been verified to work
  without it. Codex also adds
  `x-session-id`/thread headers (`realtime_conversation.rs:1334-1345`); we send
  `x-session-id: <term-llm session id>` and nothing else.
- Response: body is the raw SDP answer (`realtime_call.rs:251-257`); the call id
  is parsed from the `Location` header — last path segment starting with `rtc_`
  or a UUID (`:259-294`), e.g. `Location: /v1/live/rtc_abc`.

### Sideband (WebSocket)

- URL: `wss://api.openai.com/v1/live/{call_id}` — default base
  `https://api.openai.com/v1` (`methods.rs:60,784`), frameless normalisation to
  `/v1/live` (`methods.rs:1177-1188`), call id appended as a path segment
  (`methods.rs:1140-1169`). Overridable for tests
  (`with_webrtc_sideband_base_url`, `methods.rs:789`).
- Same auth headers as call creation; "ChatGPT-auth sessions send their bearer
  plus account id; transceiver … accept[s] that same call-create identity on
  the direct api.openai.com sideband path" (`client.rs:412-416`).
- Codex joins it right after the HTTP answer (`realtime_conversation.rs:665-689`),
  sends **no** `session.update` for frameless (`methods.rs:1000-1006`), retries
  with backoff on connect failure, and stops on 404/410 = session ended
  (`methods.rs:857-900,1035-1041`). If the socket drops mid-session it
  reconnects and retries the pending outbound message
  (`realtime_conversation/sideband.rs:135-165`).
- Codex's WebRTC host ignores `oai-events` data-channel messages
  (`codex-rs/voice-host/src/transport.rs:132`); all control traffic is on the
  sideband. Data channel: ordered, name `oai-events` (`transport.rs:112-121`).
  Offer is non-trickle: wait for ICE gathering complete before POSTing
  (`transport.rs:149-169`).

### Inbound events (frameless parser, `protocol_frameless_bidi.rs:15-95`)

| type | payload used |
|---|---|
| `session.started`, `session.updated` | session object |
| `input_transcript.added` | `item.text` (delta of the user's speech) |
| `output_transcript.added` | `item.text` (delta of the model's speech) |
| `turn.done` | `turn.role` (`user`/`assistant`), `turn.transcript` (full text) |
| `delegation.created` | `item.id`, `item.type=="delegation"`, `item.target=="client"`, concat of `item.content[].text` where `type=="input_text"` |
| `output_audio.delta` | `audio` base64 — irrelevant with WebRTC, drop |
| `error` | error text |
| anything else (`turn.created`, `turn.delta`, `session.usage.updated`, …) | logged and ignored |

Delegation request text fallback: Codex uses the content text, else the active
transcript (`realtime_conversation.rs:1750-1754`). We do the same, then fall
back to the last completed user `turn.done` transcript, then the accumulated
`input_transcript.added` deltas.

### Outbound messages (`protocol.rs:50-98`)

```json
{"type":"delegation.context.append","delegation_item_id":"item_123","channel":"speakable","content":[{"type":"input_text","text":"..."}]}
{"type":"session.context.append","channel":"speakable","content":[{"type":"input_text","text":"[USER] ..."}]}
{"type":"session.update","session":{...}}
{"type":"session.close"}
```

`channel` ∈ `speakable | commentary`. Appends are chunked at ≤500 UTF-8 bytes
on char boundaries (`methods_frameless_bidi.rs:11,109-125`). There is no
`delegation.done`; the appends are the continuation. Codex flushes streamed
agent text every 200 ms and truncates very long output head/tail with
`"\n…output truncated…\n"` (`realtime_conversation.rs:105-106,222-247`). User
typed text is prefixed `[USER] ` (`:111,829-832`).

### Prompts

- Voice model instructions: `codex-rs/prompts/templates/realtime/realtime_start.md`
  (identity, "never mention the backend", always delegate actions, `[USER]` /
  `[BACKEND]` prefixes). We ship our own shorter variant with the same rules,
  minus the Codex identity.
- Delegated turn wrapper (`codex-rs/core/src/context/realtime_delegation.rs:47-67`):

  ```
  <realtime_delegation>
    <input>…spoken request…</input>
    <transcript_delta>user: …\nassistant: …</transcript_delta>
  </realtime_delegation>
  ```

  Codex supplies its backend realtime instructions separately as developer
  context at mode transitions; they are not prepended to every user request.
  term-llm now uses the same separation, with full visual output left intact.

## Architecture decision: browser owns WebRTC, server owns auth + sideband + delegation

```
browser mic ──WebRTC audio + oai-events──▶ OpenAI (gpt-live-1-codex) ◀──WebRTC audio── browser speaker
   │                                             ▲
   │ POST /v1/live/sessions {sdp offer}          │ sideband wss://api.openai.com/v1/live/{call_id}
   ▼                                             │
term-llm serve ── POST chatgpt.com/backend-api/codex/realtime/calls (OAuth) ──┘
   │  delegation.created → startResponseRun(session) → response.output_text.delta
   │  → delegation.context.append(speakable)
   └─ /v1/events: live.* events → browser transcript/status
```

Why not terminate WebRTC in Go: `internal/webrtc` is a data-channel-only home
peer built on low-level pion (`internal/webrtc/peer.go:23-32,111-115`), with no
media tracks; adding Opus/RTP handling server-side is a large dependency and
buys nothing — the browser already has a mic, a speaker and a WebRTC stack. The
OAuth token never leaves the server; the browser only ever sees SDP.

Why not let the browser talk to OpenAI directly: call creation needs the OAuth
bearer + account id, and the sideband is where delegations arrive — both must
stay server-side so the agent runs with the server's tools, store and
approvals.

Shared contract: everything in `internal/live` is UI-agnostic. A host plugs in
a media peer (the browser today) and a `Delegator` (the serve run today); the
controller does not know which. No Go WebRTC media code is needed for v1.

## Changes

### 1. Config + capability (~60 LOC)

`internal/config/config.go` (next to `TranscriptionConfig`, `:1077`):

Follow the shape of `ImageConfig` (`:959`), `AudioConfig` (`:1023`) and
`TranscriptionConfig` (`:1078`): a top-level `provider` selector plus
provider-independent behaviour, and one sub-block per provider holding the
things whose valid values depend on that provider (credentials, model, voice,
endpoints).

```go
// LiveConfig configures live (bidirectional voice) sessions.
type LiveConfig struct {
    Enabled      bool          `mapstructure:"enabled"`      // default false
    Provider     string        `mapstructure:"provider"`     // default "chatgpt"; later "openai", "elevenlabs"
    Instructions string        `mapstructure:"instructions"` // optional override of the voice-model prompt (provider-neutral text)
    IdleTimeout  time.Duration `mapstructure:"idle_timeout"` // default 10m of no control-channel traffic
    ChatGPT      LiveChatGPTConfig `mapstructure:"chatgpt"`
}

// LiveChatGPTConfig configures gpt-live over the ChatGPT OAuth backend.
// Credentials come from the existing chatgpt_oauth.json store; there is no api_key.
type LiveChatGPTConfig struct {
    Model           string `mapstructure:"model"`             // default "gpt-live-1-codex"
    Voice           string `mapstructure:"voice"`             // default "cove" (V3 uses the V1 voice family)
    CallBaseURL     string `mapstructure:"call_base_url"`     // default https://chatgpt.com/backend-api/codex (tests)
    SidebandBaseURL string `mapstructure:"sideband_base_url"` // default https://api.openai.com/v1 (tests)
}
```

What goes where, and why:

- **Top level = "how term-llm behaves"**, independent of who provides the
  voice: the on/off switch, which provider, the prompt text we compose, idle
  handling. Later, delegation policy knobs (e.g. commentary on/off) also
  belong here. This mirrors `Transcription.SaveDir/Timestamps` and
  `Audio.OutputDir`.
- **Per-provider block = "how to reach and configure that vendor"**: model
  names (vendor-specific), voice names (vendor-specific enums), endpoints,
  and — for API-key providers — `api_key`, exactly like
  `AudioElevenLabsConfig{APIKey, Model, Voice, Format}`. `chatgpt` has no
  `api_key` because it authenticates with the stored OAuth credentials, the
  same way the `chatgpt` LLM provider does.
- `model` and `voice` are deliberately **not** top-level (unlike
  `Transcription.Model`), because a value valid for one vendor is meaningless
  for another; `Audio` already made that choice and live is closer to audio
  than to transcription.
- Provider name resolution follows `TranscribeWithConfig`
  (`internal/llm/transcribe.go:39-45`): the built-in name selects the adapter;
  a named entry in the `providers` map of the matching type may supply
  credentials/base URL for API-key providers. For `chatgpt` only the OAuth
  store is consulted.

Future providers (not implemented, but the seam is designed for them):

- `openai` — the public Realtime/gpt-live API with an API key. Same
  WebRTC-offer-in/answer-out shape but `POST /v1/realtime/calls` or the
  multipart `/v1/live` route, and the GA Realtime protocol expresses tools as
  function calls rather than `delegation.created`. Its adapter maps
  `response.function_call_arguments.done` → a delegation and
  `conversation.item.create(function_call_output)` → the append. Block:
  `LiveOpenAIConfig{APIKey, Model, Voice, BaseURL}`.
- `elevenlabs` — Conversational AI agents with "client tools"; WebRTC or
  WebSocket signalling with a signed URL minted server-side. Block:
  `LiveElevenLabsConfig{APIKey, AgentID, Voice}`.

Add `Live LiveConfig` to `Config` (next to `Transcription`, `:483`), defaults
+ `schema.go` entries, and `config_test.go` cases: defaults, explicit
`enabled: false`, and `provider: chatgpt` model/voice override.

`cmd/serve_projects.go:357 handleCapabilities`: add
`"live": {"enabled": bool, "provider": string, "model": string, "transport": "webrtc", "version": 1}`.
`enabled` requires `cfg.Live.Enabled`, `s.cfg.ui`, and the selected provider
reporting itself ready (for `chatgpt`: loadable
`credentials.LoadChatGPTCredentials`), so the button never appears when it
cannot work. Include provider and enabled in the ETag.

### 2. `internal/live` — protocol client, no serve dependencies (~450 LOC)

New package, pure Go, tested with `httptest` + gorilla server.

- `provider.go` — the seam every vendor adapter implements, so the serve
  layer, controller and frontend never see vendor specifics:

  ```go
  type Provider interface {
      Name() string                                   // "chatgpt"
      Ready(ctx context.Context) error                // credentials present, refreshable
      Start(ctx context.Context, offerSDP string, opts SessionOptions) (Session, error)
  }
  type Session interface {
      AnswerSDP() string
      Events() <-chan Event                           // normalised: Transcript, TurnDone, DelegationCreated, Error, Ended
      AppendDelegation(ctx context.Context, id string, chunk DelegationChunk) error
      AppendText(ctx context.Context, text string) error   // typed user text
      Close(ctx context.Context) error
  }
  ```

  `SessionOptions` carries the provider-neutral inputs (instructions,
  initial items, session id). `NewProvider(cfg config.LiveConfig)` switches on
  `cfg.Provider` like `TranscribeWithConfig` does; only `chatgpt` exists in
  v1. The files below are the `chatgpt` adapter.
- `chatgpt_call.go` — `CreateCall(ctx, auth, CallRequest{SDP, Session}) (CallResponse{SDP, CallID}, error)`.
  Builds the URL from `cfg.ChatGPT.CallBaseURL + "/realtime/calls?intent=quicksilver&architecture=avas"`,
  JSON body, headers from an `Auth` value (`AccessToken`, `AccountID`,
  originator/UA reused from `internal/llm/chatgpt_models.go:24-27,203-206,483` —
  export a small `llm.ChatGPTRequestHeaders(creds)` helper rather than
  duplicating). Parses `Location` per the Codex rules. Maps 401/403 to a typed
  `ErrUnauthorized` so the caller can refresh once and retry.
- `session.go` — `SessionJSON(cfg)` producing exactly the frameless object
  above (model, instructions, audio.output.voice, delegation{type:client,
  ack_filler}, initial_items).
- `events.go` — `ParseEvent([]byte) (Event, error)` for the table above plus
  `Unknown{Type}`; outbound builders `DelegationContextAppend`,
  `SessionContextAppend`, `SessionClose`, `SessionUpdate`; `ChunkText(s, 500)`.
- `chatgpt_sideband.go` — `Dial(ctx, cfg.ChatGPT.SidebandBaseURL, callID, auth) (*Sideband, error)`
  using `github.com/gorilla/websocket` (already a dependency, pattern in
  `cmd/serve_hub_reverse_connector.go:92-94`). Single writer goroutine with a
  mutex, ping/pong keepalive like `hubReversePingLoop`
  (`cmd/serve_hub_reverse.go:303`), `Events() <-chan Event`, `Send(Outbound)`,
  reconnect with capped backoff, permanent stop on 404/410 or `session.close`.
  Auth is re-derived via a callback on every dial so refreshed tokens are used.
- `controller.go` — `Controller` state machine independent of the HTTP layer:
  tracks live transcript (user/assistant deltas → completed turns), owns the
  delegation dispatcher (one active output stream, with execution follow-ups
  admitted immediately through the host's optional `SteeringDelegator`), passes structured input and
  transcript delta to the host, and exposes a `Delegator` interface. The host
  creates the bounded, escaped XML wrapper separately from persisted display text:

  ```go
  type Delegator interface {
      // Run executes the request in the bound chat session and streams output.
      // It returns when the turn finishes.
      Run(ctx context.Context, request DelegationRequest, emit func(DelegationChunk)) error
  }
  type DelegationChunk struct{ Text string; Channel string } // speakable|commentary
  ```

  The controller batches `emit` into `delegation.context.append` every 200 ms
  (or on 500-byte boundaries), with the Codex head/tail truncation policy for
  runaway output, and sends a final speakable line on error
  ("The task failed: …") so the voice model never hangs waiting.

Tests: call URL/query/headers/body/Location parsing (table-driven, incl. UUID
and missing Location); event parser table; chunker (multibyte boundary);
sideband reconnect and 410 stop with a fake gorilla server; controller with a
scripted event stream and a fake `Delegator` (delegation with empty content
falls back to the last user transcript; non-steering hosts serialize, while
voice corrections steer blocked runs without duplicate output; error path
emits a speakable failure).

### 3. Serve integration (~350 LOC)

`cmd/serve_live.go`:

- Routes (register in `cmd/serve.go:1519-1571` with `s.auth(s.cors(...))`):
  - `POST /v1/live/sessions` — body `{sdp, session_id?, initial_items?: bool}`,
    session via `resolveRequestSessionID` like `/v1/responses`
    (`cmd/serve_handlers_responses.go:265-270`). Cap SDP at 64 KiB. Requires an
    existing session (creates one through `sessionMgr.GetOrCreate`,
    `cmd/serve_session.go:237`, when `session_id` is empty — same as a fresh
    chat). One live session per chat session; a second request returns 409.
    Loads ChatGPT creds, refreshes when `IsExpired()`
    (`internal/credentials/chatgpt.go:27-31,148`), calls `live.CreateCall`,
    dials the sideband, starts the controller, and returns
    `{live_id, sdp}`. Never returns the call id or tokens to the browser.
  - `DELETE /v1/live/sessions/{live_id}` — sends `session.close`, tears down.
  - `POST /v1/live/sessions/{live_id}/text` — optional v1.1: typed text →
    `session.context.append` with `[USER] ` prefix.
- `liveSessionManager` on `serveServer`: map live_id → controller; idle timeout
  from config; closed on server shutdown alongside `sessionMgr`.
- Delegator implementation `serveLiveDelegator.Run`:
  1. `rt := s.sessionMgr.GetOrCreate(ctx, sessionID)`.
  2. Build `inputMessages` = one user message with the wrapped delegation
     prompt and `llm.Request` the same way the stateful continue path in
     `handleResponses` does after `:309` (previous response =
     `s.ensureResponseRuns().latestRun(sessionID)`,
     `cmd/serve_response_run_recovery.go:709`). **Implementation note:** the
     preparation between `handleResponses:309` and the `startResponseRun` call
     must be extracted into a helper (`s.prepareStatefulTurn`) that both the
     HTTP handler and the delegator call, rather than copied. Confirm the
     minimal set of steps needed for a first-party, existing-session,
     non-branch turn while doing this; do not re-implement project/draft/
     idempotency handling.
  3. `run, err := s.startResponseRun(rt, true, false, inputMessages, llmReq, sessionID, startResponseRunOptions{uiSession: true})`
     (`cmd/serve_response_run_stream.go:641`). `errServeSessionBusy` (a web or
     TUI turn already owns the session — see `plans/session-turn-owner.md`)
     → steer an active web response through run/epoch-fenced `InterruptMessage`;
     publish `response.steering.queued` for immediate pending UI. No second output
     subscriber is created. If no steerable web run exists, use bounded busy retries.
  4. `sub := run.subscribe(0)` (`cmd/serve_response_run_recovery.go:200`),
     consume `response.output_text.delta` → `emit(speakable)`,
     `response.tool_exec.start` → `emit(commentary: "Running <tool>…")`
     (rate-limited, one per tool group), terminal `response.completed` /
     `response.failed` / `response.cancelled` → return. `unsubscribe` on exit.
- Browser fan-out: publish `live.*` events through the existing server-event
  feed (`cmd/serve_events.go`, browser side
  `frontend/src/platform/server-events.ts:1-21,68-104` validates types, so add
  them there): `live.started`, `live.transcript` `{role, text, final}`,
  `live.delegation` `{state: queued|running|done|failed, response_id}`,
  `live.error`, `live.ended`. Because delegation turns are ordinary runs, the
  browser already sees `response.created` for them and attaches through its
  normal run-recovery path (`runEngine.recoverActiveSupervisors`,
  `frontend/src/stores/app-store.ts:853`); the live events only carry what is
  not already a run.
- Approvals: a delegated turn that needs approval blocks in the UI exactly as
  today. The delegator emits one commentary chunk "Waiting for your approval in
  the app" when it sees the approval-request event, so the voice model can say
  so instead of going silent.

Tests (`cmd/serve_live_test.go`): with `live.chatgpt.call_base_url` /
`live.chatgpt.sideband_base_url` pointed at in-process fakes and
`llm.MockProvider` scripted turns: (a) POST creates a
call with the right headers and returns the answer SDP; (b) an expired token is
refreshed before the call; (c) a `delegation.created` from the fake sideband
produces a persisted user+assistant turn in the session and the fake receives
`delegation.context.append` chunks ending with the assistant text; (d) 409 on a
second live session; (e) DELETE sends `session.close`; (f) missing creds or
`live.enabled=false` → 404/`capabilities.live.enabled=false`.

### 4. Frontend (~400 LOC + CSS)

- `frontend/src/platform/live.ts` — `LiveCall` class (mirrors
  `VoiceOperation`'s snapshot/subscribe shape, `frontend/src/platform/voice.ts:108-160`):
  `start(sessionId)` → `getUserMedia({audio:true})`, `RTCPeerConnection`
  (no ICE servers needed for a direct OpenAI call; keep a config hook), add the
  mic track, `createDataChannel('oai-events', {ordered:true})`, `createOffer`,
  wait for `icegatheringstatechange === 'complete'` (bounded 5 s), POST
  `/v1/live/sessions`, `setRemoteDescription({type:'answer', sdp})`, attach the
  remote track to a hidden `<audio autoplay>`; `stop()` → DELETE + close.
  Phases: `idle | requesting-permission | connecting | listening | speaking |
  working | failed | ended`. Data-channel messages are logged at debug level
  only. Capability check reuses `voiceCapability()` plus `RTCPeerConnection`.
- `frontend/src/api/endpoints.ts`: `liveStart`, `liveStop`, `liveText`.
- `frontend/src/stores/live-store.ts`: owns the `LiveCall`, the `liveEnabled`
  toggle (localStorage, existing storage helpers), and applies `live.*` server
  events into a small in-memory transcript (last N turns) and delegation state.
- `frontend/src/components/Composer.tsx:890-931`: when
  `capabilities.live.enabled` and the toggle is on and there is no draft text,
  the send button renders as the Live button (`send-btn live`, new `live` icon
  in `Icon.tsx`, pulsing while `listening/speaking`, spinner while `working`);
  click starts/stops the call. With draft text the button reverts to send — a
  typed message during a live call goes to `liveText` (v1.1) or falls back to a
  normal send. The toggle itself lives in the composer "+" menu next to the
  existing mic button. Disable the record-and-transcribe mic while live.
- `LiveStatus` strip modelled on `VoiceStatus` (`Composer.tsx:52-110`): phase
  copy, the current partial user/assistant transcript line, a "working…" pill
  linking to the running response, and Stop.
- CSS in `frontend/src/styles/features/composer.css` (`.send-btn.live`,
  `.live-status-*`).
- Tests: `live.test.ts` with a fake `RTCPeerConnection`/`getUserMedia` (offer →
  POST → answer applied; gathering timeout; permission denied); composer test
  for send↔live button switching; server-events test for the new event types.

### 5. Prompts (~60 LOC of text)

`internal/live/prompts.go`: `DefaultInstructions` (voice model) and
`ExecutionInstructions` (backend developer context). Delegate project work to
normal tools and approvals; keep voice output concise while allowing complete
visual code, diffs, and tables from the execution agent. Preserve safety and
approval checks rather than copying blanket refusal bans. `LiveConfig.Instructions`
replaces the conversational prompt, but authoritative host capability/session
context is appended separately. Explicit persisted mode markers allow normal
text turns to supersede live rules after Stop or a server restart.

`live_settings` exposes call-pinned inspection and optional direct-provider voice
updates. It never edits saved config and requires a provider acknowledgement;
mid-call updates may be rejected. Local protocol and tool-authority tests cover
success, rejection, cancellation, and ended/replaced calls. Live-service acceptance
of mid-call voice updates has not been verified.

## Phasing

1. **Spike (½–1 day, throwaway allowed):** config + `internal/live/call.go` +
   the `POST /v1/live/sessions` route + a minimal `LiveCall` behind a query
   flag. Goal: hear the model answer "hello" in the browser and see
   `session.started` / `turn.done` on the sideband with our OAuth token and
   `originator: term-llm`. This is the only real unknown (whether the
   `api.openai.com/v1/live/{call_id}` sideband and `chatgpt.com …/realtime/calls`
   accept term-llm's token/originator as they accept Codex's). If the sideband
   403s, try the same path under `https://chatgpt.com/backend-api/codex` before
   escalating; if call creation 403s, compare headers against a Codex CLI
   capture (`codex --debug` wire logs, target `codex_api::realtime_websocket::wire`).
2. **Server:** rest of `internal/live` with tests, session manager, DELETE,
   capabilities, server events.
3. **Frontend:** live store, button, status strip, tests.
4. **Delegation:** `prepareStatefulTurn` extraction, delegator, approval
   commentary, busy handling, end-to-end test (c).
5. **Hardening:** token refresh on reconnect, idle timeout, shutdown ordering
   (close live sessions before `sessionMgr`/store), docs page under `docs/`.
## Budget

| Area | Files | LOC |
|---|---|---|
| Config + capabilities | `config.go`, `schema.go`, `serve_projects.go` | ~60 |
| `internal/live` | `call.go`, `session.go`, `events.go`, `sideband.go`, `controller.go`, `prompts.go` | ~450 |
| Serve integration | `serve_live.go`, `serve.go`, `serve_events.go`, `serve_handlers_responses.go` (helper extraction) | ~350 |
| Frontend | `live.ts`, `live-store.ts`, `endpoints.ts`, `Composer.tsx`, `Icon.tsx`, `server-events.ts`, CSS | ~400 |
| **Total production** | | **~1,250** |

Tests (~700 LOC Go + TS) not counted. No new Go dependencies
(`gorilla/websocket` already present); no pion additions; no cgo.

## Risks and open questions

- **Auth acceptance** (spike). Codex sends extra thread/turn-metadata headers
  we omit; the comment at `client.rs:412-416` says the sideband accepts the
  call-create identity, but only the spike proves it for term-llm.
- **Model availability** on the account: `gpt-live-1-codex` may be entitlement-
  gated; surface the HTTP error text verbatim in `live.error`.
- **Delegation vs. session ownership.** The delegator uses the same lease as web
  turns, so a TUI-owned session returns busy (`plans/session-turn-owner.md`);
  it reports that case by voice and waits. Active web responses accept voice
  execution follow-ups through canonical steering. Typed Live corrections also
  use `store.steer`, bypassing voice re-delegation.
- **Spoken-only exchanges are not persisted.** The delegation prompt carries a
  `<transcript_delta>` so the agent has context, but a user who only chats with
  the voice model leaves nothing in the session. Follow-up: append completed
  `turn.done` pairs as lightweight transcript rows under the live session's
  own lease.
- **Long agent output.** Codex truncates with a head/tail policy; we copy it.
  Anything the voice model should not read aloud (diffs, tables) is already
  discouraged by the instructions, but the agent's Markdown is sent as-is.
- **Browser autoplay** policies: the `<audio>` element must be created inside
  the click handler that starts the call.
- **Hub/relay:** `/v1/live/sessions` is a normal POST and proxies fine; the
  audio path is browser↔OpenAI and never touches the Hub.

## Follow-up: TUI (deferred, findings kept so we do not redo them)

- The TUI cannot reuse the browser trick; the Go process would have to be the
  WebRTC media peer. The WebSocket transport that carries audio as base64
  (`input_audio.append` / `output_audio.delta`) requires API-key auth in Codex
  ("realtime conversation requires API key auth",
  `realtime_conversation.rs:1773-1796`); the ChatGPT OAuth path exists only for
  WebRTC calls. That is why Codex ships its `voice-host` crate.
- Cheapest viable design: `pion/webrtc/v4` for the peer (pure Go) plus
  `ffmpeg`/`ffplay` subprocesses for mic/speaker and Opus, so no cgo and no
  libopus in the binary. Codex's `voice-host/src/transport.rs:82-222` is the
  reference for offer/answer/candidate handling.
- **Measured binary cost ≈3.5 MB** (`-s -w`, go1.26, linux/amd64): scratch
  program with only the pion packages `internal/webrtc` already links = 4.6 MB;
  with `pion/webrtc/v4` + `PeerConnection` + Opus track + ogg reader/writer =
  8.0 MB; ~4.5% of the current 78.6 MB binary. The delta is mostly `srtp`,
  `rtp`, `rtcp`, `interceptor`, which any media path needs, so extending
  `internal/webrtc` ourselves would save at most ~1 MB. Isolate behind a
  build tag when we do it.
- TUI delegation would submit through `sendMessage` (`streaming.go:572`) →
  `startStream` (`:900`) so it takes the TUI turn lease; the one-owner rule in
  `plans/session-turn-owner.md` already arbitrates against web runs.
- Open items for that spike: ffmpeg device names per OS (`pulse`/`alsa`,
  `avfoundation` + macOS mic permission for the terminal app, `dshow`), and
  subprocess codec latency (~100–300 ms).

## Verification

```sh
go test ./internal/live ./internal/config -run 'Live|Config'
go test ./cmd -run 'Live|Capabilities'
make build && go vet ./... && go test ./...
npm --prefix frontend run format && npm --prefix frontend run lint && npm --prefix frontend run typecheck && npm --prefix frontend test
```

Manual (HTTPS or localhost, `live.enabled: true`, `term-llm auth login chatgpt`):
1. Toggle Live, click the Live button, allow the mic; say "hello" → hear a
   reply; `live.transcript` events show both sides.
2. Say "list the files in this project" → `live.delegation running`, a normal
   run appears in the transcript with tool calls, the model speaks a summary.
3. Ask a follow-up that needs a subagent ("have a reviewer look at main.go")
   → `spawn_agent` runs inside the delegated turn.
4. Trigger an approval-gated tool → approval prompt in the UI, voice says it is
   waiting; approve → completes.
5. Kill the network for 10 s → sideband reconnects, call continues.
6. Stop → `session.close` sent, mic released, button returns to idle;
   `DELETE` idempotent.
7. Leave a second tab open on the same session → its Live click gets 409.
