---
title: "Live voice"
weight: 8
description: "Hold a spoken conversation with the web workspace: the voice model talks, and real work runs in your ordinary chat session with the same engine, tools, and approvals."
kicker: "Browser workspace"
featured: true
next:
  label: WebRTC direct routing
  url: /guides/webrtc-direct-routing/
---

Live voice lets you hold a spoken conversation in the [web workspace](/guides/web-ui-and-api/). The voice model talks to you; when you ask for real work, it hands that work to your ordinary chat session, which runs it with the same engine, tools, approvals, and transcript as a typed message.

Live voice is off by default. Enable it in `config.yaml` and sign in to the ChatGPT provider:

```yaml
live:
  enabled: true
```

See [the `live.*` reference](/reference/configuration/#live-voice) for every live voice setting.

When live voice is available and the composer is empty, the send button becomes the **Live** button. Click it, allow microphone access, and talk. Typing while a call is open sends the text into the same conversation instead of starting a separate turn. Starting from a new chat first creates its persistent session with the selected project, provider, model, and agent, so spoken work requests have a real session to run in.

The default `chatgpt` provider connects directly using term-llm's stored OAuth credentials. It does not require a Codex executable.

## Who owns what

For the `chatgpt` and `openai` providers, the browser owns the WebRTC audio path: it captures the microphone, creates the `RTCPeerConnection`, and plays the provider's reply. The server handles only signaling and control. Gemini uses the server-proxied PCM transport described below. Both browser media implementations load only when a call starts, so browsers that never start a call do not download them.

**term-llm negotiates the call.** The `chatgpt` provider exchanges the browser's SDP offer with the ChatGPT backend, then joins the call's sideband WebSocket using the same account. The model is `gpt-live-1-codex`, using the V3/frameless protocol and its default voice, `cove`.

**term-llm owns the conversation.** When the voice model sends `delegation.created` on the sideband, term-llm's live controller passes the spoken request and recent spoken context as structured data. The host starts a normal response run in the bound session with the same engine, tools, and approvals. It batches streamed assistant text into `delegation.context.append` frames and returns them over the sideband so the model can speak the result. Tool starts and approval prompts use the commentary channel, allowing the model to explain what is happening instead of going silent.

Requests *about* the call rather than *within* the bound session never become chat turns when the control lane is enabled. A fast routing model reads every delegation first and either answers a session-management request itself or hands it to the workspace agent. Asking which sessions are running, moving the call, or changing the voice therefore leaves no message or run in the bound session and does not extend its provider history, including in the session the call leaves. With the control lane disabled—the default—there is no router, and those requests run as ordinary work in the bound session.

### Prompts, context, and controls

Delegated user bubbles show only the request, not the internal XML wrapper or instructions. term-llm persists explicit display metadata separately from provider-facing content, so the clean view survives refreshes and session reloads; it does not strip literal XML typed by a user.

Concise voice-mode rules arrive as developer context when the effective mode changes instead of being repeated in every user message. The latest mode context survives compaction. After the call ends, the next ordinary turn supersedes the live rules; restoring a session after a server restart also resets stale live context. The execution agent can produce complete visual answers, code, and diffs, while the voice model summarizes those results for speech.

At startup, the voice model receives the current provider, model, and voice; supported voices; the bound chat model, agent, project, and workspace; and a bounded tail of visible chat messages—at most 12 items and 8 KiB of text. Tool results and system or developer prompts are not copied. This host snapshot is appended even when you set custom `live.instructions`.

The execution agent uses the normal response request's configured tools, search settings, turn limits, and engine allowlists. It receives no call-scoped control tools. If a delegated turn is asked to list or switch sessions, it says that it cannot, and you repeat the request to the voice model. This failure is deliberately explicit: quietly answering a control request with a chat turn would write the exchange into the session transcript as if it were workspace work.

#### The control lane

The control lane is **off by default**. Set `live.control_plane: true` to enable it. It is separate from `live.enabled` because it adds a fast-model turn in front of every spoken request.

The flag gates only the fast-model triage turn and its tools. When it is off, no router is wired, no fast-model turn runs, and nothing reads a delegation before it enters the ordinary session lane. The delegation uses that lane's existing queue depth with no routing worker in between.

The flag does not gate actions the browser can take on a live call. The `live.session_changed` binding, `POST /v1/live/sessions/{live_id}/session`, and the web UI's in-place follow-along after a rebind need no model or router and work with the flag off. `live_settings` is also not gated: it is an ordinary registry tool whose call-scoped binding remains available to a delegated turn, so an agent configured with it can inspect or change the voice of the call driving it.

The tradeoff is latency: enabling the lane adds one fast-model turn in front of every request, including workspace work.

**The outcome follows what the turn did, not what it wrote.** If a control tool runs, the request is session management. The router answers it to the voice model and never sends it to the session lane, even if the turn then fails, exhausts its budget before summarizing, or also tries to hand the request to the workspace agent. Tool evidence takes precedence over a handoff, because handing off a control request is exactly what this lane prevents. A mixed request such as “list my sessions and also fix the parser” therefore loses its work half.

An *attempt* means a request the host could act on. Every control tool decodes its arguments strictly. An invented argument key or a missing argument returns `INVALID_PARAMS` and does not count as evidence, so the request fails open and runs as workspace work. Losing work because of a hallucinated key is worse than running it. `PERMISSION_DENIED` behaves the same way because it means the host never granted the tool, not that the model successfully requested a control action. Domain failures are different: if a switch is refused because the target already hosts another call, the host did act on a session-management request, so the refusal comes back as the answer.

An answer with no tool call behind it proves nothing. term-llm discards that answer and runs the request as workspace work instead of swallowing a workspace request and speaking an improvised reply. Everything that never reaches a verdict fails open.

The frameless realtime protocol declares no tools. Its only server-to-client request channel is `delegation.created`, which carries free text authored by the voice model. Everything the model wants from the host travels through that channel, and the host decides what the text represents.

One **fast-model routing turn** sits in front of every delegation:

- For a request about your sessions, the call, or its voice, the router handles the request and sends its final text back to the voice model on the delegation's commentary channel.
- For workspace work, it calls `pass_to_workspace`, and the delegation continues down the ordinary session lane with the same engine, tools, approvals, and transcript.

The request becomes a chat turn in the bound session only when the workspace agent should handle it. Session listing, call moves, and voice changes leave no message, run, or provider-history extension in either the current session or the session the call leaves.

The router is one inexpensive turn against an ephemeral engine with exactly five tools:

| Tool | Purpose |
| --- | --- |
| `live_settings` | Inspect the call, or request a supported voice with `{"voice":"maple"}`. Changes only the current call, never saved configuration. |
| `session_directory` | List recently active sessions or search transcripts with filters such as `{"query":"reflow","running_only":true,"project":"prj_...","include_archived":true}`. Returns at most 10 sessions with their durable number, title, running or needs-input state, and a bounded snippet. `running_only` reports the complete running set regardless of age; `include_archived` reaches sessions that are no longer active. |
| `live_switch_session` | Bind the call to an existing session by durable number or full session ID. Both `{"session":42}` and `{"session":"20260917-130000-3f5c1a9b2d4e6f70"}` are accepted. Titles, descriptions, and partial IDs are not; use `session_directory` first. |
| `live_new_session` | Start a new empty conversation and bind the call to it. Both arguments are optional. `project` and `agent` default to the current conversation's values, and a name the host cannot resolve exactly is refused with the available choices rather than guessed. |
| `pass_to_workspace` | Hand the request to the workspace agent, for example `{"request":"run the session store tests"}`. This host-local tool is not in the registry, needs no authority context, and writes only to the routing turn that created it. |

The host grants the four session tools only inside a router turn and supplies a context bound to the live call. A model cannot forge that authority: merely naming a session does not grant it, and an unauthorized call returns `PERMISSION_DENIED`. Authority stays pinned to the session the call was bound to when the turn began, so a switch during the turn cannot remove it. Ended calls reject routing, and an old call cannot affect its replacement. `session_directory`, `live_switch_session`, and `live_new_session` are not installed in delegated chat turns. Only `live_settings`, which an agent may be configured to use, keeps its binding outside the control lane.

Starting a conversation uses the same path as the browser and binds the call only after creation. The result has the same workspace resolution, provider and model defaults, and `session.created` publication as a conversation created in the UI. Rebinding uses the ordinary switch and emits the same `live.session_changed` event and host note. If creation succeeds but binding fails, the answer names the conversation, which remains available to switch to.

The routing turn is ephemeral and treats the bound session ID only as a correlation label, so it does not change any session's provider-side state. Its static system prompt remains cacheable; the request, recent spoken conversation, and current binding travel in the user message. The turn is bounded at 30 seconds, and its answer is clipped to a few hundred bytes because a spoken answer should take one or two sentences. The transport appends that answer to the voice model in 500-byte chunks.

**The handoff is short-circuited.** Most delegations are workspace work, and the router already adds one model turn. As soon as `pass_to_workspace` records the request, it cancels the routing turn; term-llm uses the recorded handoff without waiting for or reading a closing remark. A handoff therefore costs exactly one provider request. The router may repair speech recognition or remove filler, and the session lane runs that cleaned request. A session tool still takes precedence if the turn both acts and records a handoff.

**Routing is fail-open.** If no fast model resolves, the provider errors, or the router never returns, the original request goes to the session lane. It is not refused or lost. With the control plane off, requests enter the session lane directly. If a route exceeds the 45-second backstop, term-llm cancels that turn and fails open as workspace work. A router that ignores cancellation still holds the single FIFO worker until it returns, so later delegations wait. The failure is logged for the operator rather than shown to the user. If the worker's bounded queue is already full, however, the new delegation is refused instead of being queued without limit.

Answers and handoffs run off the event loop on one worker, so transcript deltas, interruptions, and delegation events continue while routing is in progress. The worker processes requests one at a time in submission order because a later request may depend on an earlier action, such as “switch back to that one.” Ending a call waits for the in-flight request. term-llm claims the delegation ID and publishes `queued` before routing, which makes a replayed provider event a no-op. A control request can therefore be answered while the bound session runs another task and is never offered to the steering path.

When a stream ends, the delegation loop drains work that is already queued. If the routing worker finishes after the loop exits, the request becomes `failed` with a reason instead of remaining `queued`, so the voice model is not left waiting for a call that has stopped answering.

The voice model does not receive the routing machinery's names, tools, or wire forms. It delegates requests it cannot answer from host context in your own words, and the host decides whether they are session management. When the control lane is enabled, the ordinary host context tells the voice model that a routing model reads requests before they become work; when it is disabled, the host makes no such promise.

**A refusal is not an error.** A refusal means the host declined a request before anything ran—for example, because four requests were already waiting to be routed. Nothing failed and there is nothing for you to fix. Only the voice model receives the reason on the delegation's commentary channel and explains it in its own words. The browser receives `live.delegation` with `state:"refused"` and no `text`; like `done`, it is terminal, so the panel returns to listening without showing an error.

A **failure** means term-llm attempted something and it went wrong, so the reason belongs to you. A session-lane error, delegation-queue overflow, or routing turn that claims a request but produces no answer reports `state:"failed"` with `text`, which the panel displays as an error. A control tool failure inside an otherwise successful routing turn is different: the router describes it, such as “that session is already driven by another live call,” and the voice model speaks the explanation.

The router reads tool JSON; the voice model does not. It hears one or two spoken sentences naming the affected session instead of a directory dump.

Voice changes require acknowledgement from the direct provider. Provider restrictions may reject a change after speech begins. term-llm reports timeouts and rejections rather than claiming success, and a supported voice name does not guarantee that a mid-call change will be accepted.

### Switching the call to another session

Switching is immediate. Because no chat turn is in flight, the call binds to the new session as soon as the control request completes. The session it leaves is no longer driven by voice, and its next typed turn sees the ordinary live-mode reset. The provider's `x-session-id` remains the session where the call started; it is a correlation label and is not renegotiated mid-call.

A switch is refused when the target does not exist, is a subagent child session, is already driven by another live call, or the call has ended. These are execution failures rather than form refusals, so both the voice model and panel receive the reason. Archived targets are allowed and reported; `session_directory` reaches them with `include_archived`.

Name a target by its durable number or full session ID. A partial ID is rejected rather than resolved because every session ID begins with a timestamp and a truncated value could silently match the wrong session. A target with a task already running is allowed, but the next spoken request is queued as **guidance to that running task** rather than answered directly. The switch result names that task's last request so the voice model can describe it. After a successful switch, the host appends a short note to the voice conversation naming the new session because the model received the original `session_id` when the call started. The new session's history is not re-snapshotted.

A request that combines lookup and work takes two spoken steps. For example, first ask to find and switch to the reflow session, then ask to summarize what you decided there. The router resolves the session; the workspace agent never guesses. The same pattern applies to work with no prior history: first ask to start a new conversation, then ask it to write the test.

### Starting a new conversation

Say “start a new conversation in the term-llm project” or “new chat with the reviewer agent” to create a conversation and move the call in one step. `project` and `agent` are optional and default to the current conversation, so “start a new conversation” keeps the current project and agent. Agent names match the registry case-insensitively and are reported in the registry's spelling. On a server pinned to one agent with `--agent`, that agent wins; a conflicting request is refused rather than silently overridden.

A project name must resolve to exactly one project: term-llm tries an exact ID, then a case-insensitive exact name, then a unique case-insensitive name substring. An unknown or ambiguous name is refused with the candidates because the project controls which files the conversation may access.

The new conversation is an ordinary persistent row with the same workspace resolution, provider and model defaults, and `session.created` event as one created in the browser. It starts **empty**: nothing carries over from the previous conversation, which keeps its transcript and stops being driven by voice. The ordinary switch then binds the call, emitting `live.session_changed` and a host note. If the conversation is created but rebinding fails, the answer names it instead of discarding it. It remains in the sidebar, ready for another switch request.

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

The default `gpt-live-1` uses the public Live API: JSON session creation at `/v1/live/sessions`, followed by an authenticated sideband at `/v1/live/sessions/{session_id}/attach`. It delegates execution to the same controller as ChatGPT, receives transcript fragments, and returns speakable results and quiet progress through native Live context appends. Typed input runs through the execution backend and is mirrored into the voice session as context. Current-call voice changes are not supported; choose the voice before starting the call.

An explicitly configured model whose name contains `realtime`, such as `gpt-realtime`, uses the Realtime API with `/v1/realtime/calls`, Realtime function calling, and a complete function result after the execution turn. `gpt-live-1` is never sent in Realtime mode. Both transports use API-key authentication and keep browser media on WebRTC.

### Gemini API-key provider

Gemini Live uses a same-origin, server-proxied PCM transport so the Gemini API key never reaches the browser:

```yaml
live:
  enabled: true
  provider: gemini
  gemini:
    api_key: ${GEMINI_API_KEY}
    model: gemini-3.8-live
    voice: Kore
```

The server opens Gemini's `BidiGenerateContent` WebSocket, configures audio plus input and output transcription and the execution-delegation function, seeds bounded visible history, and translates Gemini tool calls for the same live controller used by the other providers. Microphone input is mono signed 16-bit little-endian PCM at 16 kHz. Gemini output is signed 16-bit little-endian PCM at 24 kHz.

The browser captures input through an `AudioWorklet`, resamples it to 16 kHz, and begins uploading after about 100 ms of audio. It then drains accumulated microphone audio in bounded, sequential `POST /v1/live/sessions/{live_id}/audio/input` requests of at most 64 KiB. Provider output returns as base64 PCM and interruption markers on `GET /v1/live/sessions/{live_id}/audio/output`.

One continuous output `AudioWorklet` consumes the 24 kHz PCM from a bounded FIFO. It preserves interpolation phase across provider packet boundaries when the audio context runs at 44.1 or 48 kHz and passes samples through directly at 24 kHz. It does not create an `AudioBufferSourceNode` for each packet. An interruption resets the FIFO immediately. Both routes derive from `live_id` and use the normal authenticated API client, so its public prefix, [Hub](/guides/hub/) authentication, and WebRTC HTTP tunnel hooks apply.

The HTTP split lets Gemini Live work through a deployed reverse Hub connector, which supports finite requests and streamed HTTP/SSE responses but not WebSocket upgrades or an infinite bidirectional request body. SSE output plus finite microphone posts fit that transport. Direct clients can explicitly request the legacy `/audio` WebSocket with `websocket_pcm`.

The authenticated start response returns a short-lived, one-use media capability. Only the output attachment can consume it, and only one HTTP or WebSocket output consumer can attach. Microphone posts are accepted only while that output remains attached. A disconnect, stop, provider failure, or attach timeout closes the provider call and rejects later input.

`live.gemini.api_key` falls back to `GEMINI_API_KEY`, then `GOOGLE_API_KEY`. `live.gemini.base_url` is an advanced test or proxy seam; leave it at the default for normal configurations.

Gemini's `interimInputTranscription` appears as a replaceable preview while you speak. Corrections replace the preview instead of appending duplicate words. Only authoritative input transcriptions enter conversation history and delegation context.

For opt-in browser audio counters, provider wire diagnostics, and PCM artifact capture, see [live voice diagnostics](/guides/debugging/#live-voice-diagnostics).

## Behaviour worth knowing

Each chat session supports one live session. A second browser tab that clicks **Live** on the same session receives `409`; rebinding onto a session already driven by another call is also refused.

An idle delegation starts an ordinary response run. While a web response is running, delegated follow-ups enter the **standard steering queue** immediately. The UI shows the recognized request as pending guidance, and it becomes a user transcript entry when the agent consumes it. Corrections neither cancel the task nor wait for its entire answer. Only execution handoffs are steered, not partial speech or conversational greetings.

Control requests are the exception. term-llm classifies them before creating a session queue entry, so they can be answered while the bound session runs a task and are never offered to steering. A dedicated control worker handles one request at a time in submission order, preventing a control turn from delaying transcript, interruption, or delegation delivery. A control request counts as call activity, so a call containing only control requests is not reaped as idle.

Typed text during a running live task uses the same steering channel directly instead of passing through the voice model again. The original delegation remains the one spoken-output stream; follow-up admission is acknowledged without replaying that answer. Steering uses normal persistence, cancellation, and run-ownership checks. A terminal-owned session without a web response uses bounded busy retries.

Runaway output is not read aloud in full. The model speaks the beginning and end of a long reply with a truncation notice between them.

Spoken-only exchanges are not written to the transcript. Delegated turns are ordinary runs, so they appear in the web UI like typed messages, including tool calls and approvals. Control requests leave no trace in any chat session, including the one the call leaves; they appear only as delegation entries in the Live panel. They do cost one inexpensive routing turn per delegation when the control lane is enabled.

A call with no control-channel traffic for `live.idle_timeout`, which defaults to ten minutes, closes automatically. Closing the browser tab or pressing **Stop** ends the call and releases the microphone; `DELETE` is idempotent. Switching sessions in the web UI keeps the call open and rebinds it. If the rebind is refused, the client stops the call instead of leaving it attached to a session you have left.

The ChatGPT and OpenAI sidebands reconnect with backoff and re-resolve credentials, so a short control-channel outage does not end those conversations. Gemini media uses one stateful upstream WebSocket; if it disconnects, the call ends cleanly and can be started again.

PCM playback preserves queued speech when the provider generates faster than real time. A single output worklet retains at most 10 minutes of 24 kHz audio and 16,384 packet descriptors; exceeding either resource limit ends the call with a visible playback-capacity error rather than silently dropping speech. Interrupting, stopping, disposing, or failing a call resets or disconnects the audio queue immediately, and stale acknowledgements from an older playback epoch are ignored.

## HTTP surface

| Route | Purpose |
| --- | --- |
| `POST /v1/live/sessions` | Start a call. WebRTC providers exchange an SDP offer and answer; Gemini returns an HTTP PCM capability, or the legacy direct WebSocket URL when explicitly requested. |
| `GET /v1/live/sessions/{live_id}/audio/output` | Stream Gemini PCM and interruption events over SSE; consumes the short-lived, one-use media capability. |
| `POST /v1/live/sessions/{live_id}/audio/input` | Send a finite batch of mono 16 kHz PCM microphone audio to Gemini; accepted only while output is attached. |
| `POST /v1/live/sessions/{live_id}/diagnostics` | Submit authenticated, bounded, fixed-schema browser audio counters for debugging. Unavailable unless `serve --debug`, `--debug-raw`, or `TERM_LLM_LIVE_DEBUG` enables diagnostics. See [live voice diagnostics](/guides/debugging/#live-voice-diagnostics). |
| `GET /v1/live/sessions/{live_id}/audio` | Attach a legacy direct-client Gemini PCM WebSocket. It shares the one-consumer capability and is not used through reverse Hub. |
| `DELETE /v1/live/sessions/{live_id}` | End the call; idempotent. |
| `POST /v1/live/sessions/{live_id}/text` | Inject typed text into the running conversation. |
| `POST /v1/live/sessions/{live_id}/session` | Rebind the call with `{"session_id":"42"}` for a durable number or a full session ID. Returns `200 {"live_id","session_id","session_number","title","no_op"}`; `409` when another live call drives the target or the call has ended; `404` for an unknown `live_id`; and `400` for an invalid body, unknown target, or partial session ID. |
| `GET /v1/live/sessions/{live_id}/events` | Stream `live.started`, `live.transcript`, `live.delegation`, `live.interrupted`, `live.error`, `live.session_changed`, and `live.ended` over SSE, with `?after=` replay. |

`live.session_changed` carries `{"session_id","session_number","title"}` and is emitted once per real rebind from the same lock that performs the change, so racing switches cannot arrive out of order. A no-op switch emits nothing. `live.started` and every `live.delegation` event also carry `session_id`, read under that lock when publishing the event. A concurrent rebind therefore cannot leave a later event naming the session the call just left. If a reconnect occurs after the 512-event ring buffer has evicted the change, a later event can still recover the current binding.

`live.delegation` carries `{"delegation_id","state"}` and includes `text` when there is something to send. Its sequence is `queued`, `running`, and exactly one terminal state: `done`, `refused`, or `failed`. Only `failed` carries text—the reason the work did not happen—which the panel renders as an error. `done` and `refused` carry no text. A refusal means the host declined the form or routing of a control request; correction is addressed to the voice model and remains deliberately invisible in the panel.

The two non-terminal states distinguish waiting from active work. Every delegation is published as `queued` as soon as the host accepts it because nothing is executing it yet. It waits for routing, and if the router hands it to the workspace agent while a task is already running, it continues waiting in the bound session. The router or session lane publishes `running` when it starts executing the request. Guidance folded into a running task is steered rather than run, so it moves from `queued` directly to `done` without reporting `running`.

Only the call's transcript and delegation state travel on the live event stream. Delegated turns reach the browser through the normal response-run path, so an open call does not flood the shared server-event feed.

Microphone HTTP uploads adapt to request latency. Audio captured during an in-flight request joins the next request, preserving order without a fixed 100 ms throughput ceiling. The waiting queue is bounded to five seconds; exceeding it fails the call visibly instead of silently dropping speech.

The output worklet prebuffers 150 ms of audio before starting or resuming after an empty queue. If less audio arrives, it releases that audio after at most 250 ms of render time measured from the first queued sample; later packets do not restart the deadline. Interruptions discard the buffer immediately. This adds a small initial delay to absorb delivery jitter without delaying every packet or waiting indefinitely for a short response.
