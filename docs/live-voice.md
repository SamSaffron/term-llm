# Live voice

Live voice is a spoken conversation with the web interface. The voice model talks to you; when you ask for real work it hands that work to your ordinary chat session, which runs it with the same engine, tools, approvals and transcript as a typed message.

Live voice is off by default. Enable it in `config.yaml` and sign in to the ChatGPT provider:

```yaml
live:
  enabled: true
```

When live voice is available and the composer is empty, the send button becomes the **Live** button; click it, allow the microphone, and talk. Typing while a call is open sends the text into the same conversation instead of starting a separate turn. Starting from a new chat first creates its persistent session with the selected project, provider, model, and agent, so spoken work requests have a real session to run in.

The default `chatgpt` provider connects directly using term-llm's stored OAuth credentials. No Codex executable is required.

## Who owns what

The browser owns the audio path. It captures the microphone, creates the `RTCPeerConnection`, and plays the model's reply; the server never handles media, and the WebRTC implementation is a lazily loaded chunk, so browsers that never start a call never download it.

**term-llm negotiates the call.** The `chatgpt` provider exchanges the browser's SDP offer with the ChatGPT backend, then joins the call's sideband WebSocket using the same account. The model is `gpt-live-1-codex`, using the V3/frameless protocol and its default voice, `cove`.

**term-llm owns the conversation.** The voice model's `delegation.created` arrives on the sideband. The controller in `internal/live` passes the spoken request and recent spoken context as structured data; the host starts a normal response run in the bound session — same engine, same tools, same approvals. Streamed assistant text is batched into `delegation.context.append` frames and returned over the sideband so the model can speak the result. Tool starts and approval prompts go on the commentary channel so it can say what is happening instead of going silent.

### Prompts, context, and controls

Delegated user bubbles show only the request, not the internal XML wrapper or instructions. Explicit display metadata is persisted separately from provider-facing content, so the clean view survives refreshes and session reloads; literal XML typed by a user is not stripped.

Concise voice-mode rules are supplied as developer context when the effective mode changes, not repeated in each user message. The latest mode context survives compaction. After the call ends, the next ordinary turn supersedes the live rules; restoring a session after a server restart also resets stale live context. The execution agent can produce complete visual answers, code, and diffs. The voice model summarizes those results for speech.

At startup, the voice model receives the current provider/model/voice, supported voices, bound chat model/agent/project/workspace, and a bounded tail of visible chat messages (at most 12 items and 8 KiB of text). Tool results and system/developer prompts are not copied. This host snapshot is appended even with custom `live.instructions`.

The execution agent uses the normal response request's configured tools, search settings, turn limits, and engine allowlists. A narrow **`live_settings`** tool additionally lets a delegated turn inspect the call or request a supported voice with `{"voice":"maple"}`. It changes only the originating call, never saved configuration. Ordinary turns, other sessions, and subagents do not acquire control merely because the tool is registered. Ended calls reject control; old calls cannot affect replacement calls.

Voice changes require an acknowledgement from the direct provider. Provider restrictions may reject changes after speech begins; timeouts and rejections are reported rather than claiming a successful switch. Supported voice names do not guarantee that mid-call changes are accepted.

### OpenAI API-key provider

To use the public OpenAI GPT-Live API instead of a ChatGPT subscription:

```yaml
live:
  enabled: true
  provider: openai
  openai:
    api_key: sk-... # or set OPENAI_API_KEY in the server environment
    model: gpt-live-1
    voice: marin
```

The API key stays on the server. This provider uses separately billed OpenAI API access, not ChatGPT OAuth or a Codex executable. `live.chatgpt.*` settings do not apply; in particular, `juniper` is a ChatGPT voice, not a public OpenAI Live voice.

The default `gpt-live-1` uses the public Live API: JSON session creation at `/v1/live/sessions`, then an authenticated sideband at `/v1/live/sessions/{session_id}/attach`. It uses client delegation to the same execution controller as ChatGPT, receiving transcript fragments and returning speakable results and quiet progress through native Live context appends. Typed input runs through the execution backend and is mirrored into the voice session as context. Current-call voice changes are not supported; choose the voice before starting the call.

For an explicitly configured model whose name contains `realtime` (for example `gpt-realtime`), term-llm retains the legacy Realtime API: `/v1/realtime/calls`, Realtime function calling, and a complete function result after the execution turn. `gpt-live-1` is never sent in Realtime mode. Both transports use API-key authentication and keep browser media on WebRTC.

## Behaviour worth knowing

One live session per chat session. A second browser tab that clicks Live on the same session is refused with `409`.

An idle delegation starts an ordinary response run. While a web response is running, delegated follow-ups enter the **standard steering queue** immediately: the UI shows the recognized request as pending guidance, and it becomes a user transcript entry when the agent consumes it. Corrections do not cancel the task or wait for its entire answer. Only execution handoffs are steered—not partial speech or conversational greetings.

Typed text during a running Live task uses the same steering channel directly, rather than passing through the voice model a second time. The original delegation remains the single spoken-output stream; follow-up admission is acknowledged without replaying that answer. Steering uses normal persistence, cancellation, and run ownership checks. A terminal-owned session without a web response still uses bounded busy retries.

Runaway output is not read aloud in full: the head and tail of a long reply are spoken with a truncation notice between them.

Spoken-only exchanges are not written to the transcript. Delegated turns are ordinary runs, so they appear in the web UI exactly like typed messages, with their tool calls and approvals.

A call with no control-channel traffic for `live.idle_timeout` (default ten minutes) is closed. Closing the browser tab, switching sessions, or pressing Stop ends the call and releases the microphone; `DELETE` is idempotent.

The direct provider's sideband reconnects with backoff and re-resolves credentials, so a short network outage does not end the conversation. A provider session that has genuinely ended (`404`/`410`) stops it for good.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `live.enabled` | `false` | Master switch; also gates the capability the web UI reads. |
| `live.provider` | `chatgpt` | `chatgpt` uses OAuth; `openai` uses the public API with an API key. |
| `live.instructions` | *(unset)* | Replaces the conversational prompt; host capability/session context is still appended. |
| `live.idle_timeout` | `10m` | Idle time before the server closes a call. |
| `live.openai.api_key` | *(unset)* | OpenAI API key; falls back to `OPENAI_API_KEY`. |
| `live.openai.model` | `gpt-live-1` | Public GPT-Live model; explicitly named `realtime` models select the legacy Realtime transport. |
| `live.openai.voice` | `marin` | Live: `alloy`, `ash`, `ballad`, `beacon`, `bossa`, `cedar`, `cinder`, `coral`, `delta`, `echo`, `gleam`, `marin`, `meridian`, `quartz`, `ripple`, `sage`, `shimmer`, `stone`, `tempo`, `verse`, `vesper`, or `willow`. Legacy Realtime supports only `alloy`, `ash`, `ballad`, `coral`, `echo`, `sage`, `shimmer`, `verse`, `marin`, and `cedar`. |
| `live.openai.base_url` | `https://api.openai.com/v1` | Public API base for call creation and sideband. |
| `live.chatgpt.model` | `gpt-live-1-codex` | Voice model for the V3/frameless protocol. |
| `live.chatgpt.voice` | `cove` | V3 voice: `juniper`, `maple`, `spruce`, `ember`, `vale`, `breeze`, `arbor`, `sol`, or `cove`. |
| `live.chatgpt.call_base_url` | `https://chatgpt.com/backend-api/codex` | Call-creation endpoint; overridden in tests. |
| `live.chatgpt.sideband_base_url` | `https://api.openai.com/v1` | Control-channel endpoint; overridden in tests. |

**For `chatgpt` only: do not use the V2 voices `marin` or `cedar` with this protocol.** ChatGPT V3 uses the V1 voice family, not the V2 family. Sending `marin` to this endpoint was verified to return the misleading `403 "Voice session access denied."`; changing only the voice to `cove` allowed the same account and direct client to connect and receive audio. term-llm validates the voice before making a request. If an older configuration explicitly sets `marin`, remove that override or select a supported voice.

`live.enabled` alone is not enough for the button to appear: `/v1/capabilities` reports `live.enabled` only when the feature is on, the request is first-party, and provider readiness succeeds. Account or workspace restrictions may still prevent a call; valid ChatGPT credentials alone do not guarantee model access.

Live voice needs a secure context (HTTPS or localhost) because the browser will not grant microphone access otherwise.

## HTTP surface

| Route | Purpose |
| --- | --- |
| `POST /v1/live/sessions` | Exchange an SDP offer for the provider's answer and bind the call to a chat session. |
| `DELETE /v1/live/sessions/{live_id}` | End the call; idempotent. |
| `POST /v1/live/sessions/{live_id}/text` | Inject typed text into the running conversation. |
| `GET /v1/live/sessions/{live_id}/events` | SSE stream of `live.started`, `live.transcript`, `live.delegation`, `live.error`, `live.ended`, with `?after=` replay. |

Only the call's own transcript and delegation state travel on that stream; delegated turns reach the browser through the normal response-run path, so an open call does not flood the shared server-event feed.

## Hosting-UI client tools (opt-in)

A trusted hosting web UI can register named actions that the voice model calls
without sending the request through the execution agent. This is an extension
hook, not a built-in navigation feature: a “room” in the example below is an
application view, not a speaker or physical device.

**Supported mode:** `live.provider: openai` with an explicitly configured
Realtime model (for example `live.openai.model: gpt-realtime`). Default public
GPT-Live (`gpt-live-1`) and ChatGPT (`gpt-live-1-codex`) expose delegation rather
than this named function-call contract and **do not support these tools**.
Registering a nonempty tool list in either mode makes call startup fail with a
clear HTTP 400; it does not silently change providers or add an agent roundtrip.
Normal live calls with no registrations continue to work in all existing modes.

Register from trusted application code before starting Live, using the hosting
app's `LiveStore` instance (available as `app.liveStore` in the Preact app):

```ts
await app.liveStore.registerClientTools([
  {
    name: 'ui_navigate',
    description: 'Switch the hosting application to a named room view.',
    parameters: {
      type: 'object',
      properties: { room: { type: 'string', enum: ['overview', 'kitchen'] } },
      required: ['room'],
      additionalProperties: false,
    },
    execute(args, { signal }) {
      // Schemas guide the model; the host must validate before side effects.
      if (
        Object.keys(args).length !== 1 ||
        (args.room !== 'overview' && args.room !== 'kitchen')
      ) throw new Error('Unknown UI room');
      signal.throwIfAborted();
      router.navigate(`/rooms/${args.room}`); // your application router
      return { room: args.room };
    },
  },
]);
```

`registerClientTools` replaces the registration list; pass `[]` to remove it.
Await registration before starting a call. Registration is rejected during a
call or after store disposal. Definitions are snapshotted for each call; callback
closures can read current application state. Lower-level hosts can pass the same
list as `new LiveCall(startEndpoint, stopEndpoint, { clientTools })`; their start
callback must forward its optional third argument as `client_tools` in the
existing `POST /v1/live/sessions` request. Only schemas are sent, never callbacks.
This is a source-level hosting API, not a global script injection API or a
cross-origin widget/postMessage bridge.

### Validation, results, and lifetime

- Names must be unique and match `ui_[A-Za-z0-9_]{1,60}`. Built-in tools cannot be
  replaced. There are at most 16 registrations, with 1–1024-byte descriptions
  and object schemas containing `properties`, at most 16 KiB per schema. The
  existing total live-start HTTP body limit still applies.
- Schemas are provider-facing JSON Schema, not a client-side validation engine.
  The hook validates the JSON/object envelope and size; **the trusted handler
  must validate fields, application permissions, and allowed destinations**.
  Avoid accepting URLs, script text, or arbitrary commands. No model-provided
  JavaScript is evaluated. These actions do not acquire server tool permissions
  or run through server approval dialogs; the host must ask for confirmation
  when appropriate.
- `execute(args, { signal, callId })` may return JSON-serializable data or a
  promise. Results use `{ ok: true, result }`; failures use `{ ok: false, error }`.
  Arguments and encoded results are limited to 16 KiB. Thrown/rejected errors,
  invalid JSON, unknown tools, serialization errors, and the fixed 10-second
  handler timeout return correlated error results. Return only data safe to send
  to the voice provider; handler error messages are also sent (bounded, no stack).
- Only completed Realtime responses dispatch actions. Call IDs deduplicate
  execution within one live call. Multiple function calls in one response wait
  for all results, including built-in delegation, before one continuation.
  In opted-in calls the browser owns function-result continuation; the server
  still runs built-in delegation normally. Unregistered tools never invoke a
  host callback. A call is bounded to 1,024 tracked function calls/responses;
  restart Live if that limit is reached.
- Stop, session switch, failed transport, and disposal abort pending callbacks
  and discard late results. Async handlers must honor `signal` and check it
  before side effects after an `await`: JavaScript promises cannot be forcibly
  stopped and already completed UI changes cannot be undone by this hook.
  Registrations are local to the hosting store, not persisted or shared across
  tabs; calls are not replayed after reload.

### Performance boundary

No registrations means no client-tool data-channel listener, handler timeout,
new polling, extra HTTP request, or function-continuation change. Tests assert
that the default call sends only the original start arguments, installs no
message handler, sends no data-channel frames, and leaves no new timers in the
fake browser. The default server Realtime function-result sequence is also
covered separately from opted-in continuation ownership.

With registrations, schemas ride the existing startup request and results use
the existing ordered WebRTC data channel. Each completed tool response uses the
normal function-result continuation, not an additional execution-agent/model
roundtrip. Microphone capture, audio tracks, playback, and media transport are
unchanged. This is a structural constraint backed by mocked protocol tests,
**not** a claim of measured zero overhead: registration adds code/schema bytes
and opted-in control-event parsing/dispatch. Real-provider/WebRTC audio latency
and browser lifecycle behavior still need live integration testing.
