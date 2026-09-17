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

For the `chatgpt` and `openai` providers, the browser owns the WebRTC audio path: it captures the microphone, creates the `RTCPeerConnection`, and plays the provider's reply, while the server handles only signaling and control. Gemini uses a different, server-proxied PCM transport described below. Both browser media implementations are lazily loaded, so browsers that never start a call do not download them.

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

### Gemini API-key provider

Gemini Live uses a same-origin, server-proxied PCM transport so the Gemini API key is never sent to the browser:

```yaml
live:
  enabled: true
  provider: gemini
  gemini:
    api_key: ${GEMINI_API_KEY}
    model: gemini-3.8-live
    voice: Kore
```

The server opens Gemini's `BidiGenerateContent` WebSocket, configures audio plus input/output transcription and the execution-delegation function, seeds bounded visible history, and translates Gemini tool calls into the same controller used by the other live providers. Microphone input is mono signed 16-bit little-endian PCM at 16 kHz; Gemini output is signed 16-bit little-endian PCM at 24 kHz.

The browser captures through an input `AudioWorklet`, resamples to 16 kHz, and starts uploads after about 100 ms of audio, then drains accumulated microphone audio in bounded (at most 64 KiB), sequential `POST /v1/live/sessions/{live_id}/audio/input` requests. Provider output returns as base64 PCM and interruption markers on `GET /v1/live/sessions/{live_id}/audio/output`. One continuous output `AudioWorklet` consumes the 24 kHz PCM from a bounded FIFO; it preserves interpolation phase across provider packet boundaries when the actual audio context runs at 44.1 or 48 kHz (and passes samples directly at 24 kHz). It does not create an `AudioBufferSourceNode` per packet. An interruption resets that FIFO immediately. Both routes are generated from `live_id` and use the normal authenticated API client, so its public prefix, Hub authentication, and WebRTC HTTP tunnel hooks apply.

The HTTP split is intentional: a deployed reverse Hub connector supports finite requests and streamed HTTP/SSE responses, but not WebSocket upgrades or an infinite bidirectional request body. SSE output plus finite microphone posts therefore works through existing Hubs without a Hub restart or reverse-protocol upgrade. The older same-origin `/audio` WebSocket remains available for direct clients that explicitly request `websocket_pcm`.

The authenticated start response returns a short-lived, one-use media capability. Only the output attachment may consume it, only one HTTP or WebSocket output consumer may attach, and microphone posts are accepted only while that output remains attached. Disconnect, stop, provider failure, or attach timeout closes the provider call and rejects later input.

`live.gemini.api_key` falls back to `GEMINI_API_KEY`, then `GOOGLE_API_KEY`. `live.gemini.base_url` is an advanced test/proxy seam; normal configurations should leave it at its default.

Gemini's `interimInputTranscription` updates appear as a replaceable while-you-speak preview. Corrections replace the preview rather than appending duplicate words; only authoritative input transcriptions enter conversation history and delegation context.

### Opt-in Gemini Live diagnostics

Diagnostics are completely off by default. To investigate Gemini audio gaps, start the server explicitly with metadata diagnostics:

```sh
term-llm serve web --debug
```

`--debug` logs Gemini setup timing, incoming event kinds and inter-event timing, input/interim/output transcription counts, PCM chunk byte sizes and cumulative counts, turn/interruption boundaries, and connection errors/close reasons. It does **not** log PCM base64, prompts, seeded history, authentication values, media capabilities, or API keys. Server diagnostics are written to the `term-llm serve` process's stderr; keep that terminal open or inspect the service manager's captured stderr.

For sanitized inbound Gemini event JSON as well, use the global raw-debug flag (it also enables the metadata diagnostics):

```sh
term-llm serve web --debug-raw
```

You can instead enable diagnostics only for live sessions using an environment variable (preserve your usual server/Hub arguments):

```sh
TERM_LLM_LIVE_DEBUG=metadata term-llm serve web
TERM_LLM_LIVE_DEBUG=raw term-llm serve web
```

`1`, `true`, `on`, and `metadata` enable metadata diagnostics; `raw` also enables sanitized raw events. Values are case-insensitive and whitespace is ignored. Unset, `0`, `false`, `off`, and unrecognized values do not enable diagnostics. Existing `--debug` / `--debug-raw` flags remain additive and are not disabled by the variable. Set it in the node process's environment and restart the node; no config edit or Hub restart is needed.

**Raw diagnostics are sensitive:** sanitized raw events may include input and output transcript text. PCM payloads are replaced by decoded byte lengths, tool arguments and prompt/history containers are redacted, and keys, authorization values, bearer tokens, media capability tokens, and session-resumption handles are redacted. Do not publish a raw log without reviewing it. Provider and browser reports carry the same `live_id` for correlation.

While either flag or the environment opt-in is active, open the browser's developer tools and select **Console**. Lines prefixed with `[live]` report the Web Audio context state and a five-second cumulative batch of packets received, enqueued into the output worklet (`scheduled`), and actually consumed (`ended`), plus FIFO playback seconds, worklet underrun episodes, interruption flush counts (the legacy buffer-reset counter stays zero), microphone queue drops, and input POST latency/inflight counts. Worklet consumption acknowledgements are aggregated rather than emitted for every render quantum. Output/input silence ages are observations only: ordinary silence on an open stream does not fail or restart a healthy call. The same fixed numeric report is posted through the authenticated, per-call `/v1/live/sessions/{live_id}/diagnostics` route and appears in server stderr as `[live browser]`.

The reporting route is unavailable unless live diagnostics are enabled, accepts no arbitrary browser messages or URLs, is size/rate/lifetime bounded, and stops with the call. With neither flags nor the environment opt-in enabled, the response has no diagnostic opt-in, so the browser creates no diagnostic timer and makes no diagnostic requests.

#### Opt-in PCM artifact capture

When investigating a beep or other rendering artifact on the Gemini PCM transport, you can additionally capture the exact provider PCM received by the browser and the output of the browser's Web Audio playback graph. This is a second, explicit browser opt-in and works only when the server start response has diagnostics enabled as described above. In DevTools **Console**, run:

```js
localStorage.setItem("term-llm-live-pcm-capture", "1");
```

Then start a new Gemini Live call and reproduce the problem. The diagnostics module is loaded only after both the server diagnostics flag and this local opt-in are present. It is not collected for WebRTC calls. After stopping the call, inspect availability and download the artifacts from the Console:

```js
window.termLLMLivePCMArtifacts.status();
await window.termLLMLivePCMArtifacts.download();
```

Downloading during a call now stops only the diagnostic capture, not the call itself. The download waits for the recorder’s final data before exporting (with a bounded timeout reported in the metadata), so an immediate post-stop download includes the final audio tail.

The download produces:

- `*-provider-mono24k.wav`: the incoming signed 16-bit little-endian mono 24 kHz provider PCM, with a standard WAV header;
- `*-browser-rendered.webm`, `.ogg`, or `.mp4` when `MediaRecorder` supports recording a `MediaStreamAudioDestinationNode` and produced data; and
- `*-timings.json`: incoming and retained byte/sample counts, chunk receipt and output-worklet enqueue boundaries, interruption boundaries, leading/trailing samples, minima/maxima, rendered chunk boundaries, limits, truncation flags, and any recorder support error.

The rendered recording is a tee from the single output worklet inside the Web Audio graph. **The microphone source and input worklet are never connected to it.** Capture remains entirely in browser memory until the user invokes the download function; term-llm does not upload these artifacts or put their contents in browser/server diagnostic reports. The recording describes Web Audio's rendered graph, not downstream operating-system, Bluetooth, DAC, amplifier, or speaker behavior.

Capture is bounded to 60 seconds. Incoming PCM retention is capped at 2,880,000 bytes (60 seconds of mono PCM16 at 24 kHz), browser-rendered encoded data at 8 MiB, and detailed chunk/interruption arrays at 4,096 entries. Reaching a cap is recorded in the JSON rather than growing memory without limit. Only the newest call's capture is retained; starting another opted-in call replaces the previous capture. Stopping or failing a call stops the recorder and media-destination tracks but keeps the bounded artifacts available for download. If `MediaRecorder` or `MediaStreamAudioDestinationNode` is unsupported, the call continues normally and the provider WAV plus JSON remain available.

When finished, remove the opt-in and optionally clear the retained in-memory capture:

```js
localStorage.removeItem("term-llm-live-pcm-capture");
window.termLLMLivePCMArtifacts?.clear();
```

Reloading the page also clears retained artifacts. Treat both recordings as sensitive voice data and review them before sharing.

## Behaviour worth knowing

One live session per chat session. A second browser tab that clicks Live on the same session is refused with `409`.

An idle delegation starts an ordinary response run. While a web response is running, delegated follow-ups enter the **standard steering queue** immediately: the UI shows the recognized request as pending guidance, and it becomes a user transcript entry when the agent consumes it. Corrections do not cancel the task or wait for its entire answer. Only execution handoffs are steered—not partial speech or conversational greetings.

Typed text during a running Live task uses the same steering channel directly, rather than passing through the voice model a second time. The original delegation remains the single spoken-output stream; follow-up admission is acknowledged without replaying that answer. Steering uses normal persistence, cancellation, and run ownership checks. A terminal-owned session without a web response still uses bounded busy retries.

Runaway output is not read aloud in full: the head and tail of a long reply are spoken with a truncation notice between them.

Spoken-only exchanges are not written to the transcript. Delegated turns are ordinary runs, so they appear in the web UI exactly like typed messages, with their tool calls and approvals.

A call with no control-channel traffic for `live.idle_timeout` (default ten minutes) is closed. Closing the browser tab, switching sessions, or pressing Stop ends the call and releases the microphone; `DELETE` is idempotent.

The ChatGPT/OpenAI sideband reconnects with backoff and re-resolves credentials, so a short control-channel outage does not end those conversations. Gemini media uses one stateful upstream WebSocket; if it disconnects, the call ends cleanly and can be started again.

## Configuration

| Key                              | Default                                            | Meaning                                                                                                                                                                                                                                                                                                                                           |
| -------------------------------- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `live.enabled`                   | `false`                                            | Master switch; also gates the capability the web UI reads.                                                                                                                                                                                                                                                                                        |
| `live.provider`                  | `chatgpt`                                          | `chatgpt` uses OAuth; `openai` and `gemini` use their public APIs with server-side API keys.                                                                                                                                                                                                                                                      |
| `live.instructions`              | _(unset)_                                          | Replaces the conversational prompt; host capability/session context is still appended.                                                                                                                                                                                                                                                            |
| `live.idle_timeout`              | `10m`                                              | Idle time before the server closes a call.                                                                                                                                                                                                                                                                                                        |
| `live.openai.api_key`            | _(unset)_                                          | OpenAI API key; falls back to `OPENAI_API_KEY`.                                                                                                                                                                                                                                                                                                   |
| `live.openai.model`              | `gpt-live-1`                                       | Public GPT-Live model; explicitly named `realtime` models select the legacy Realtime transport.                                                                                                                                                                                                                                                   |
| `live.openai.voice`              | `marin`                                            | Live: `alloy`, `ash`, `ballad`, `beacon`, `bossa`, `cedar`, `cinder`, `coral`, `delta`, `echo`, `gleam`, `marin`, `meridian`, `quartz`, `ripple`, `sage`, `shimmer`, `stone`, `tempo`, `verse`, `vesper`, or `willow`. Legacy Realtime supports only `alloy`, `ash`, `ballad`, `coral`, `echo`, `sage`, `shimmer`, `verse`, `marin`, and `cedar`. |
| `live.openai.base_url`           | `https://api.openai.com/v1`                        | Public API base for call creation and sideband.                                                                                                                                                                                                                                                                                                   |
| `live.gemini.api_key`            | _(unset)_                                          | Gemini API key; falls back to `GEMINI_API_KEY`, then `GOOGLE_API_KEY`.                                                                                                                                                                                                                                                                            |
| `live.gemini.model`              | `gemini-3.8-live`                                  | Gemini Live model used by `BidiGenerateContent`.                                                                                                                                                                                                                                                                                                  |
| `live.gemini.voice`              | `Kore`                                             | Prebuilt Gemini voice. Common choices include `Kore`, `Puck`, `Charon`, `Fenrir`, `Aoede`, `Leda`, `Orus`, and `Zephyr`.                                                                                                                                                                                                                          |
| `live.gemini.base_url`           | Gemini `v1beta` `BidiGenerateContent` WSS endpoint | Advanced proxy/test override.                                                                                                                                                                                                                                                                                                                     |
| `live.chatgpt.model`             | `gpt-live-1-codex`                                 | Voice model for the V3/frameless protocol.                                                                                                                                                                                                                                                                                                        |
| `live.chatgpt.voice`             | `cove`                                             | V3 voice: `juniper`, `maple`, `spruce`, `ember`, `vale`, `breeze`, `arbor`, `sol`, or `cove`.                                                                                                                                                                                                                                                     |
| `live.chatgpt.call_base_url`     | `https://chatgpt.com/backend-api/codex`            | Call-creation endpoint; overridden in tests.                                                                                                                                                                                                                                                                                                      |
| `live.chatgpt.sideband_base_url` | `https://api.openai.com/v1`                        | Control-channel endpoint; overridden in tests.                                                                                                                                                                                                                                                                                                    |

**For `chatgpt` only: do not use the V2 voices `marin` or `cedar` with this protocol.** ChatGPT V3 uses the V1 voice family, not the V2 family. Sending `marin` to this endpoint was verified to return the misleading `403 "Voice session access denied."`; changing only the voice to `cove` allowed the same account and direct client to connect and receive audio. term-llm validates the voice before making a request. If an older configuration explicitly sets `marin`, remove that override or select a supported voice.

`live.enabled` alone is not enough for the button to appear: `/v1/capabilities` reports `live.enabled` only when the feature is on, the request is first-party, and provider readiness succeeds. Account or workspace restrictions may still prevent a call; valid ChatGPT credentials alone do not guarantee model access.

Live voice needs a secure context (HTTPS or localhost) because the browser will not grant microphone access otherwise.

PCM playback preserves queued speech when the provider generates faster than real time. A single output worklet retains at most 10 minutes of 24 kHz audio and 16,384 packet descriptors; exceeding either resource limit ends the call with a visible playback-capacity error rather than silently dropping speech. Interrupting, stopping, disposing, or failing a call resets or disconnects the FIFO immediately, and stale worklet acknowledgements from an older playback epoch are ignored.

## HTTP surface

| Route                                          | Purpose                                                                                                                                                                  |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `POST /v1/live/sessions`                       | Start a call: WebRTC providers exchange an SDP offer/answer; Gemini returns an HTTP PCM capability (or the legacy direct WebSocket URL when explicitly requested).       |
| `GET /v1/live/sessions/{live_id}/audio/output` | Gemini PCM/interrupt SSE output; consumes the short-lived one-use media capability.                                                                                      |
| `POST /v1/live/sessions/{live_id}/audio/input` | Gemini finite mono 16 kHz PCM microphone batch; accepted only while output is attached.                                                                                  |
| `POST /v1/live/sessions/{live_id}/diagnostics` | Debug-only, authenticated, bounded fixed-schema browser audio counters; unavailable unless `serve --debug`, `--debug-raw`, or `TERM_LLM_LIVE_DEBUG` enables diagnostics. |
| `GET /v1/live/sessions/{live_id}/audio`        | Legacy direct-client Gemini PCM WebSocket; shares the same one-consumer capability and is not used through reverse Hub.                                                  |
| `DELETE /v1/live/sessions/{live_id}`           | End the call; idempotent.                                                                                                                                                |
| `POST /v1/live/sessions/{live_id}/text`        | Inject typed text into the running conversation.                                                                                                                         |
| `GET /v1/live/sessions/{live_id}/events`       | SSE stream of `live.started`, `live.transcript`, `live.delegation`, `live.interrupted`, `live.error`, `live.ended`, with `?after=` replay.                               |

Only the call's own transcript and delegation state travel on that stream; delegated turns reach the browser through the normal response-run path, so an open call does not flood the shared server-event feed.

Microphone HTTP uploads adapt to request latency: audio captured during an in-flight request is sent together in the next request, preserving order without a fixed 100 ms throughput ceiling. The waiting queue is bounded to five seconds; exceeding it fails the call visibly rather than silently deleting pieces of speech.

The output worklet prebuffers 150 ms of audio before starting or resuming after an empty queue. If less audio arrives, it releases it after at most 250 ms of render time measured from the first queued sample; later packets do not restart that deadline. Interruptions discard the buffer immediately. This adds a small initial delay to absorb delivery jitter, without delaying every packet or waiting indefinitely for a short response.
