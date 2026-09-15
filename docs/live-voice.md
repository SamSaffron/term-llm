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

Voice changes require an acknowledgement from the direct provider. Provider restrictions may reject changes after speech begins; timeouts and rejections are reported rather than claiming a successful switch. Supported voice names do not guarantee that mid-call changes are accepted. The optional Codex transport does not expose voice changes.

### Optional Codex transport

`live.provider: codex` instead negotiates through an isolated, ephemeral `codex app-server` process. This requires an executable on the server's PATH (`live.codex.path`, default `codex`); term-llm does not install or download it. The child logs in using term-llm's OAuth tokens without writing them to Codex storage or inheriting the user's Codex config. Client-managed handoffs are relayed through the browser's data channel. This is an alternative transport, not an authorization workaround. It uses Codex's default voice and the fixed `gpt-live-1-codex` model; `live.chatgpt.*` overrides apply only to the direct provider.

## Behaviour worth knowing

One live session per chat session. A second browser tab that clicks Live on the same session is refused with `409`.

An idle delegation starts an ordinary response run. While a web response is running, delegated follow-ups enter the **standard steering queue** immediately: the UI shows the recognized request as pending guidance, and it becomes a user transcript entry when the agent consumes it. Corrections do not cancel the task or wait for its entire answer. Only execution handoffs are steered—not partial speech or conversational greetings.

Typed text during a running Live task uses the same steering channel directly, rather than passing through the voice model a second time. The original delegation remains the single spoken-output stream; follow-up admission is acknowledged without replaying that answer. Steering uses normal persistence, cancellation, and run ownership checks. A terminal-owned session without a web response still uses bounded busy retries.

Runaway output is not read aloud in full: the head and tail of a long reply are spoken with a truncation notice between them.

Spoken-only exchanges are not written to the transcript. Delegated turns are ordinary runs, so they appear in the web UI exactly like typed messages, with their tool calls and approvals.

The optional Codex transport has a hard fifteen-minute call lifetime and a four-call limit per server.

A call with no control-channel traffic for `live.idle_timeout` (default ten minutes) is closed. Closing the browser tab, switching sessions, or pressing Stop ends the call and releases the microphone; `DELETE` is idempotent.

The direct provider's sideband reconnects with backoff and re-resolves credentials, so a short network outage does not end the conversation. A provider session that has genuinely ended (`404`/`410`) stops it for good.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `live.enabled` | `false` | Master switch; also gates the capability the web UI reads. |
| `live.provider` | `chatgpt` | Direct ChatGPT OAuth transport; `codex` selects the optional subprocess transport. |
| `live.codex.path` | `codex` | Executable name resolved on PATH, or an absolute path. Used only by the Codex transport. |
| `live.instructions` | *(unset)* | Replaces the conversational prompt; host capability/session context is still appended. |
| `live.idle_timeout` | `10m` | Idle time before the server closes a call. |
| `live.chatgpt.model` | `gpt-live-1-codex` | Voice model for the V3/frameless protocol. |
| `live.chatgpt.voice` | `cove` | V3 voice: `juniper`, `maple`, `spruce`, `ember`, `vale`, `breeze`, `arbor`, `sol`, or `cove`. |
| `live.chatgpt.call_base_url` | `https://chatgpt.com/backend-api/codex` | Call-creation endpoint; overridden in tests. |
| `live.chatgpt.sideband_base_url` | `https://api.openai.com/v1` | Control-channel endpoint; overridden in tests. |

**Do not use the V2 voices `marin` or `cedar` with this protocol.** Codex V3 uses the V1 voice family, not the V2 family. Sending `marin` to this endpoint was verified to return the misleading `403 "Voice session access denied."`; changing only the voice to `cove` allowed the same account and direct client to connect and receive audio. term-llm validates the voice before making a request. If an older configuration explicitly sets `marin`, remove that override or select a supported voice.

`live.enabled` alone is not enough for the button to appear: `/v1/capabilities` reports `live.enabled` only when the feature is on, the request is first-party, and provider readiness succeeds. Account or workspace restrictions may still prevent a call; valid ChatGPT credentials alone do not guarantee model access.

Live voice needs a secure context (HTTPS or localhost) because the browser will not grant microphone access otherwise.

## HTTP surface

| Route | Purpose |
| --- | --- |
| `POST /v1/live/sessions` | Exchange an SDP offer for the provider's answer and bind the call to a chat session. |
| `DELETE /v1/live/sessions/{live_id}` | End the call; idempotent. |
| `POST /v1/live/sessions/{live_id}/text` | Inject typed text into the running conversation. |
| `POST /v1/live/sessions/{live_id}/signal` | Relay a browser data-channel control frame for the optional Codex transport. |
| `GET /v1/live/sessions/{live_id}/events` | SSE stream of `live.started`, `live.transcript`, `live.delegation`, `live.signal`, `live.error`, `live.ended`, with `?after=` replay. |

Only the call's own transcript and delegation state travel on that stream; delegated turns reach the browser through the normal response-run path, so an open call does not flood the shared server-event feed.
