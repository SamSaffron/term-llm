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

Requests *about* the call rather than *within* the bound session never become chat turns once the control lane is switched on: every delegation is then read by a fast routing model first, which answers a session-management request itself or hands it to the workspace agent, so asking which sessions are running, moving the call elsewhere, or changing the voice leaves no message, no run in the bound session, and no extension of its provider history, including in the session the call departs from. With the lane switched off — the default — there is no such reader and those requests are ordinary work in the bound session.

### Prompts, context, and controls

Delegated user bubbles show only the request, not the internal XML wrapper or instructions. Explicit display metadata is persisted separately from provider-facing content, so the clean view survives refreshes and session reloads; literal XML typed by a user is not stripped.

Concise voice-mode rules are supplied as developer context when the effective mode changes, not repeated in each user message. The latest mode context survives compaction. After the call ends, the next ordinary turn supersedes the live rules; restoring a session after a server restart also resets stale live context. The execution agent can produce complete visual answers, code, and diffs. The voice model summarizes those results for speech.

At startup, the voice model receives the current provider/model/voice, supported voices, bound chat model/agent/project/workspace, and a bounded tail of visible chat messages (at most 12 items and 8 KiB of text). Tool results and system/developer prompts are not copied. This host snapshot is appended even with custom `live.instructions`.

The execution agent uses the normal response request's configured tools, search settings, turn limits, and engine allowlists. It is offered no call-scoped control tools: a delegated turn that is asked to list or switch sessions says plainly that it cannot, and the user repeats the request to the voice model. That failure is deliberately loud, because the quiet alternative — answering a control request with a chat turn — writes the exchange into the session's transcript as if it were work.

#### The control lane

**Off by default.** Set `live.control_plane: true` to switch it on; it is opt-in separately from `live.enabled` because it puts a model turn in front of every spoken request.

What the flag gates is exactly the **fast-model triage turn and the tools that belong to it**: with it off, no router is wired at all, no fast-model turn is made, nothing is read before it runs, and every delegation is ordinary work for the bound session — exactly how the voice surface behaved before this lane existed. The delegation then enters the session lane directly, resolved on the event loop as it always was, with the delegation queue's own depth and no routing worker in between.

What it does not gate is everything a browser can do to a live call. The `live.session_changed` binding, `POST /v1/live/sessions/{live_id}/session`, and the web UI's in-place follow-along when the call is rebound are a UI feature: they need no model, no triage, and no router, and they work with the flag off. `live_settings` is likewise not gated — it is an ordinary registry tool whose call-scoped binding a delegated turn keeps, so a conversation configured with it can still inspect or change the voice of the call driving it.

The cost to understand before enabling it is latency: it adds one fast-model turn in front of every request, work included.

**The outcome follows what the turn did, not what it wrote.** A control tool that ran settles the question — the request was session management — so it is answered to the voice model and never reaches the session lane, even when the turn then fails, runs out of budget before summarising, or passes the same request to the workspace agent anyway. Evidence outranks a handoff, because a handoff of a control request is precisely the outcome this lane exists to prevent; a mixed request ("list my sessions and also fix the parser") therefore loses its work half, which is the direction the invariant already chose.

An *attempt* here means a request the host could act on. Every control tool decodes its arguments strictly, so a call the model got wrong — an invented argument key, a missing one — returns `INVALID_PARAMS` and is not evidence: the request falls open and runs as work, because losing a workspace request to a hallucinated key is worse than running it. The same is true of `PERMISSION_DENIED`, which means the host never granted the tool rather than that the model asked for something. Domain failures are different: a switch refused because the target already hosts another call is a session-management request the host declined, and comes back as an answer.

Conversely an answer with no tool call behind it proves nothing, so it is discarded and the request runs as work: swallowing a workspace request and speaking an improvised reply in its place is the worse of the two mistakes. Fail-open still applies to everything that never reached a verdict.

The frameless realtime protocol declares no tools at all: its only server-to-client request channel is `delegation.created`, carrying free text the voice model authored. Everything the voice model wants from the host travels that one channel, so the host reads the text and decides what it is.

One **fast-model routing turn** sits in front of every delegation:

- it handles the request itself when it is about the user's sessions, this call, or the call's voice, and its final text is spoken back to the voice model on the delegation's commentary channel;
- otherwise it hands the request to the **workspace agent** with `pass_to_workspace`, and the delegation continues down the ordinary session lane — same engine, same tools, same approvals, same transcript.

Either way the request never becomes a chat turn in the bound session unless the workspace agent is the one that should do it. Asking which sessions are running, moving the call elsewhere, or changing the voice leaves no message, no run, and no extension of the bound session's provider history, including in the session the call departs from.

The router is one cheap turn against a throwaway `llm.Engine` with exactly five tools:

| Tool | Purpose |
| --- | --- |
| `live_settings` | Inspect the call, or request a supported voice with `{"voice":"maple"}`. Changes only the call, never saved configuration. |
| `session_directory` | Read-only: list recently active sessions or search transcripts (`{"query":"reflow","running_only":true,"project":"prj_...","include_archived":true}`). Returns at most 10 sessions with their durable number, title, running/needs-input state, and a bounded snippet. A `running_only` request reports the whole running set regardless of how old those sessions are, and `include_archived` is what reaches a session that is no longer active. |
| `live_switch_session` | Bind the call to another existing session by durable number or full session id (`{"session":42}` and `{"session":"20260917-130000-3f5c1a9b2d4e6f70"}` are both accepted). Titles, descriptions, and partial ids are not accepted; find the session with `session_directory` first. |
| `live_new_session` | Start a brand-new empty conversation and bind the call to it. Both arguments are optional — `project` and `agent` default to the current conversation's, so plain "start a new conversation" stays where the user is — and a name the host cannot resolve exactly is refused with the available ones rather than guessed. |
| `pass_to_workspace` | Hand the request to the workspace agent (`{"request":"run the session store tests"}`). Host-local: it is in no registry, needs no authority context, and writes only to the recorder of the turn that built it. |

The four session tools are the same implementations the delegated path once exposed, executed inside the router's turn with a context the host builds itself: `llm.ContextWithSessionID` plus those four bindings, and nothing else. Only the host constructs that context, and only while `live.control_plane` is on, so a model cannot forge it — a tool called with a context that merely names a session is denied with `PERMISSION_DENIED` — and no registered schema grants anything. The authority is pinned to the session the call was bound to when the turn started, so a switch mid-turn cannot strip it. Ended calls reject routing, old calls cannot affect replacement calls, and `session_directory`, `live_switch_session` and `live_new_session` are never installed anywhere else: a delegated chat turn cannot list sessions, start a conversation, or move the call, and only `live_settings` — a tool an agent may legitimately be configured with — keeps its binding outside this lane.

Starting a conversation creates it through the same path the browser uses and only then binds the call to it, so the new row is indistinguishable from one created in the UI — same workspace resolution, same provider and model defaults, same `session.created` publication — and the rebind is the ordinary switch, with the same `live.session_changed` event and host note. If creation succeeds and the bind fails, the answer says so and names the conversation, which is still there to switch to.

The turn is `Ephemeral` and treats the bound session id as a correlation label only, so nothing it does reaches any session's provider-side state. Its system prompt is static — providers can cache it — and the request, the recent spoken conversation, and the current binding travel in the user message. The turn is bounded at 30 seconds, and the answer is clipped to a few hundred bytes because a spoken answer is one or two sentences and the transport appends to the voice model in 500-byte chunks.

**The handoff is short-circuited.** Almost every delegation is workspace work, and the router already adds a model turn in front of it, so the common path must not pay for a second one. The moment `pass_to_workspace` records the request it cancels the routing turn from inside itself, and the host treats the recorded handoff as the decision: the router's closing remark is never waited for and never read. A handoff therefore costs exactly one provider request. The router may tidy the text on the way through — speech recognition repair and filler removal are free, since it has read the request anyway — and the tidied request is what the session lane runs. A recorded handoff is still only read when the turn reached for no session tool: evidence outranks it, so a turn that both acts and hands the request over is answered as a control request and its handoff is ignored.

**Routing is fail-open.** No fast model to resolve, a provider error, or a router that never returns: the delegation goes to the session lane with its **original** text. It is never refused and never lost. A host with no router at all never routes anything — with the control plane off the request is placed on the session lane directly, with no worker and no refusal to fall open from. A routing turn that runs past the 45-second backstop has its **turn cancelled** and the request falls open as work; the backstop does not release the worker, so a router that ignores its own cancellation still holds the FIFO worker until it returns, and later delegations wait behind it. The failure is logged for the operator and not surfaced to the user, because the user asked for something and a broken triage step is not theirs to fix. The one refusal this lane still produces is a delegation that arrives when the single routing worker is already at capacity; the worker is bounded and FIFO, and it refuses rather than queueing without limit.

Answers and handoffs are produced **off the event loop** by that single worker, so the caller keeps receiving transcript deltas, interruptions, and delegation events while a route runs. Requests are routed one at a time, in submission order, because a later request may depend on an earlier one having acted ("switch back to that one"). The worker joins the session's wait group, so ending a call waits for the request in flight. The event loop claims the delegation id — which is what makes a replayed provider event a no-op — and publishes the delegation as `queued` before handing it over. Nothing here consults the bound session's runtime: a request about the call is answered while a task is running there, and it is never offered to the steering path.

The delegation queue is never closed. It has two producers — the event loop, which places unrouted requests on it, and the routing worker — and a send on a closed channel panics even inside a `select` with a default, because the close can land between the readiness check and the send. Ending the stream is signalled separately instead: the delegation loop drains what is already queued and exits. A request the route worker finishes after that point is reported `failed` with the reason rather than left `queued`, because nothing remains to run it and a delegation the voice model is still waiting on is a call that has stopped answering.

The voice model is not told any of the routing machinery's names, tools, or wire forms. It delegates every request it cannot answer from host context, in the user's own words, with nothing to prepend, and the host decides what is session management. The one thing the host does state — and only when the lane is on, because only then is it true — is that a routing model reads those requests before they become work: that sentence travels through the ordinary host-context channel (`SessionOptions.Context`), not through the static prompt, so a host with the flag off promises nothing it will not do. Asking the voice model to recognise a session control was tried twice and failed both times, on the first phrasing the host had not been taught to anticipate; reading the request is the classification, and the router is the reader.

**A refusal is not an error.** It is the host declining a request before anything ran — a request that arrived when four were already waiting to be routed. Nothing failed and nothing is the user's to fix, so only the voice model is told: it hears the reason on the delegation's commentary channel (the only place `controlRejectionText` appears) and explains in its own words. The browser hears none of it — a refused delegation arrives as `live.delegation` with `state:"refused"` and **no `text`**, terminal exactly like `done`, so the panel returns to listening with nothing to read and no error.

A **failure** is different: something was attempted and went wrong, and the reason belongs to the user. A session-lane run that errored, a delegation-queue overflow, and a routing turn that claimed the request but produced no answer all report `state:"failed"` **with** `text`, which the panel shows as its error line. A tool that failed *inside* an otherwise successful routing turn is not one of these: the router reports it in words ("that session is already driven by another live call") and the voice model speaks that. Misrouting the two audiences is what makes a working system look broken: a refusal rendered as an error tells the user that guidance meant for the model is a mistake they made.

The router reads the tools' JSON, so the voice model never does: it hears one or two spoken sentences naming the session that was acted on, inside the answer budget, instead of a directory dump.

Voice changes require an acknowledgement from the direct provider. Provider restrictions may reject changes after speech begins; timeouts and rejections are reported rather than claiming a successful switch. Supported voice names do not guarantee that mid-call changes are accepted.

### Switching the call to another session

Switching is immediate. No chat turn is in flight, so the call is bound to the new session as soon as the control request completes, and the session it left is no longer driven by voice (its next typed turn sees the ordinary live-mode reset). The provider's `x-session-id` remains the session the call started with — it is a correlation label and is not renegotiated mid-call.

A switch is refused when the target does not exist, is a subagent (child) session, is already driven by another live call, or when the call has ended. Those are execution failures, not form refusals: the voice model hears why, and so does the panel, because the user asked for a move the host could not make. Archived targets are allowed and reported, and `session_directory` reaches them with `include_archived`. A target must be named by its durable number or its full session id: a partial id is rejected rather than resolved, because every session id starts with a timestamp and a truncated id would silently match a different session. A target with a task already running is allowed, but the next spoken request is queued as **guidance to that running task** instead of being answered directly, and the switch result names that task's last request so the voice model can describe it. After a successful switch the host appends a short note to the voice conversation naming the new session, because the model was told the original `session_id` at call start; the new session's history is deliberately not re-snapshotted.

A request that needs both a lookup and real work ("summarise what we decided in the reflow session") is two steps for the voice model: one spoken request that finds and switches to the session, handled by the router, and then the work as an ordinary delegation in the session that is now bound. The router resolves the session; the workspace agent is never asked to guess at one. The same two-step shape covers work with no history to attach to ("start a new conversation and write the test"), where the first request starts and binds the conversation and the second runs in it.

### Starting a new conversation

"Start a new conversation in the term-llm project" or "new chat with the reviewer agent" creates a conversation and moves the call to it in one step. `project` and `agent` are optional and default to the current conversation's, so plain "start a new conversation" stays where the user is, run by the agent they are already with. A named agent is matched against the agent registry case-insensitively and reported back in the registry's own spelling; on a server pinned to one agent (`--agent`), that agent wins and a conflicting request is refused rather than silently overridden. A named project must resolve to exactly one project — exact id first, then case-insensitive exact name, then a unique case-insensitive substring of the name — and an unknown or ambiguous name is refused with the candidates, because a project decides which files the conversation may touch.

The conversation is created through the same path the browser uses, so it is an ordinary row: same workspace resolution, same provider and model defaults, published as `session.created` so every sidebar picks it up. It starts **empty** — nothing is carried over from the conversation the user was in, which keeps its transcript and stops being driven by voice — and the call is then bound to it through the ordinary switch, so `live.session_changed` and the host note arrive exactly as they do for a switch. If the conversation is created and the rebind fails, the answer names the conversation instead of discarding it: it is still in the sidebar, and asking to switch to it is enough.

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

One live session per chat session. A second browser tab that clicks Live on the same session is refused with `409`, and a rebind onto a session another call already drives is refused too.

An idle delegation starts an ordinary response run. While a web response is running, delegated follow-ups enter the **standard steering queue** immediately: the UI shows the recognized request as pending guidance, and it becomes a user transcript entry when the agent consumes it. Corrections do not cancel the task or wait for its entire answer. Only execution handoffs are steered—not partial speech or conversational greetings.

Control requests are the exception to both rules. They are classified before the queue exists for them, so they are answered even while the bound session has a task running, and they are never offered to the steering path. The answer is produced on a control worker of its own — one request at a time, in submission order — so a control turn never delays transcript, interruption, or delegation delivery. A control request counts as call activity, so an all-control call is not reaped as idle.

Typed text during a running Live task uses the same steering channel directly, rather than passing through the voice model a second time. The original delegation remains the single spoken-output stream; follow-up admission is acknowledged without replaying that answer. Steering uses normal persistence, cancellation, and run ownership checks. A terminal-owned session without a web response still uses bounded busy retries.

Runaway output is not read aloud in full: the head and tail of a long reply are spoken with a truncation notice between them.

Spoken-only exchanges are not written to the transcript. Delegated turns are ordinary runs, so they appear in the web UI exactly like typed messages, with their tool calls and approvals. Control requests leave no trace in any chat session — not even the one the call leaves behind — and appear only as delegation entries in the Live panel. They do cost one cheap routing turn in front of every delegation, which is the price of the host knowing what a spoken request is about.

A call with no control-channel traffic for `live.idle_timeout` (default ten minutes) is closed. Closing the browser tab or pressing Stop ends the call and releases the microphone; `DELETE` is idempotent. Switching sessions in the web UI keeps an open call and rebinds it (the voice-initiated switch above); if that rebind is refused, the client stops the call rather than leaving a call bound to a session the user has left.

The ChatGPT/OpenAI sideband reconnects with backoff and re-resolves credentials, so a short control-channel outage does not end those conversations. Gemini media uses one stateful upstream WebSocket; if it disconnects, the call ends cleanly and can be started again.

## Configuration

| Key                              | Default                                            | Meaning                                                                                                                                                                                                                                                                                                                                           |
| -------------------------------- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `live.enabled`                   | `false`                                            | Master switch; also gates the capability the web UI reads.                                                                                                                                                                                                                                                                                        |
| `live.provider`                  | `chatgpt`                                          | `chatgpt` uses OAuth; `openai` and `gemini` use their public APIs with server-side API keys.                                                                                                                                                                                                                                                      |
| `live.instructions`              | _(unset)_                                          | Replaces the conversational prompt; host capability/session context is still appended.                                                                                                                                                                                                                                                            |
| `live.control_plane`             | `false`                                            | Route every delegation through a fast-model triage turn that can list or search sessions, move the call, and change its voice. Opt-in: it adds a model turn to every request. It gates that turn and its tools only — the call's session binding, the switch endpoint, and the UI follow-along work without it — and a control request the turn proved is session management is answered or reported, never run in the session lane. See "The control lane".                             |
| `live.control_provider`          | _(unset)_                                          | Provider for the control lane's triage turn. Unset follows the provider's fast model, which is shared with auto-titling and interrupt classification.                                                                                                                                                                                              |
| `live.control_model`             | _(unset)_                                          | Model for that turn. Naming a model on its own keeps the conversation's own provider. Point this at a model that calls tools reliably: one that answers in prose instead simply sends the request to the workspace agent.                                                                                                                          |
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
| `POST /v1/live/sessions/{live_id}/session`     | Rebind the call: `{"session_id":"42"}` (durable number) or a full session id. `200 {"live_id","session_id","session_number","title","no_op"}`; `409` when another live call already drives the target or the call has ended; `404` for an unknown `live_id`; `400` for an invalid body, an unknown target, or a partial session id. |
| `GET /v1/live/sessions/{live_id}/events`       | SSE stream of `live.started`, `live.transcript`, `live.delegation`, `live.interrupted`, `live.error`, `live.session_changed`, `live.ended`, with `?after=` replay.      |

`live.session_changed` carries `{"session_id","session_number","title"}` and is emitted once per real rebind, from inside the lock that performs it, so two racing switches cannot arrive out of order. A no-op switch emits nothing. Both `live.started` and every `live.delegation` event also carry `session_id`, read under the same lock that publishes the event so a concurrent rebind can never leave a later event naming the session the call has just left; a client that reconnects after the 512-event ring buffer evicted the change can therefore still recover the current binding.

`live.delegation` carries `{"delegation_id","state"}` and a `text` when there is one to send. The sequence is `queued`, `running`, and then exactly one terminal state: `done`, `refused`, or `failed`. `queued` and `running` describe the request being worked on. Of the terminal states only `failed` carries text — the reason the work did not happen, which the panel renders as its error line; `done` and `refused` carry none. A `refused` delegation is the host declining a control request's form or routing, whose correction is addressed to the voice model and is therefore deliberately invisible to the panel.

The two non-terminal states are not decoration, and the difference between them is the only thing that distinguishes a request that is waiting from one that is being worked on. Every delegation is published as `queued` the moment the host takes it, because nothing is executing it yet: it is waiting to be routed, and a request the router hands to the workspace agent then keeps waiting if a task is already running in the bound session. `running` is published by whichever step actually executes it — the session lane when it starts the turn, or the router when it answered the request itself. A request folded into a running task as guidance is steered rather than run, so it reports `queued` and then `done` without ever reporting `running`.

Only the call's own transcript and delegation state travel on that stream; delegated turns reach the browser through the normal response-run path, so an open call does not flood the shared server-event feed.

Microphone HTTP uploads adapt to request latency: audio captured during an in-flight request is sent together in the next request, preserving order without a fixed 100 ms throughput ceiling. The waiting queue is bounded to five seconds; exceeding it fails the call visibly rather than silently deleting pieces of speech.

The output worklet prebuffers 150 ms of audio before starting or resuming after an empty queue. If less audio arrives, it releases it after at most 250 ms of render time measured from the first queued sample; later packets do not restart that deadline. Interruptions discard the buffer immediately. This adds a small initial delay to absorb delivery jitter, without delaying every packet or waiting indefinitely for a short response.
