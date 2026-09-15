# Companion live-call bridge

This is an opt-in backend for `SamSaffron/jarvis-app`, not a web UI feature or a
new term-llm model provider. Existing APIs and defaults are unchanged.

## Enable

Install a compatible Codex executable yourself, authenticate ChatGPT using
term-llm's existing native OAuth configuration (`term-llm auth login chatgpt`),
and run, for example:

```sh
term-llm serve api --live-codex-path /path/to/codex
```

An empty `--live-codex-path` (the default) disables calls. A configured name such
as `codex` is resolved on the server's PATH; an explicit absolute path is
recommended. The server does not install or download Codex. Missing executables
or OAuth credentials fail this endpoint with 503, without disabling other APIs.
`--auth none` cannot enable live calls. Use the existing serve bearer token
(`Authorization: Bearer ...`; `x-api-key` is also supported), or an authenticated
serve passkey session. Existing CORS policy applies. No ChatGPT credential is
sent to the app.

## HTTP contract

`POST {base}/v1/live/calls`, where `{base}` is the existing serve base path
(default `/ui`; it can be configured empty). Content-Type must be
`application/json`. The JSON object is:

```json
{
  "sdp": "v=0\r\n...complete browser-generated SDP offer...",
  "instructions": "App-owned realtime instructions",
  "initial_items": [
    {"role": "user", "text": "Previous user message"},
    {"role": "assistant", "text": "Previous assistant reply"}
  ]
}
```

The app must create an audio WebRTC offer and its realtime events data channel.
Only `sdp` is required. Omitted instructions are passed as an empty prompt, not
replaced with term-llm's agent instructions. Initial items are optional,
role-bearing text history, **not** arbitrary Realtime API item objects.

Limits (bytes, not characters):

- Entire encoded request: **131,072 bytes**.
- SDP: **65,536 bytes**, starts with `v=0\r\n`, parses as SDP, and contains an
  audio media description.
- Instructions: **16,384 bytes**.
- Initial items: **128 items**, **8,192 total text bytes**. This deliberately
  conservative limit stays below Codex's 8,192 estimated-text-token limit.
- Item roles: `user` or `assistant`; text must be nonblank.
- UTF-8 text only; control characters other than CR, LF, TAB are rejected.
- Unknown fields, duplicate keys (including in items), null values, trailing
  JSON values and excessive nesting are rejected. No model, tool, executable,
  server URL, credential, or filesystem overrides are accepted from the app.

Success is **200** with `Content-Type: application/json` and
`Cache-Control: no-store`:

```json
{
  "protocol": "frameless-bidi-v3",
  "delegation_completion": "context_append",
  "sdp": "v=0\r\n...remote SDP answer...",
  "expires_at": "2026-09-15T06:00:00Z"
}
```

Apply `sdp` as an RTCSessionDescription of type `answer`. `protocol` pins
Frameless Bidi V3, and `delegation_completion` confirms that client-managed
handoffs continue when the client sends `delegation.context.append` frames.
`expires_at` is an
RFC3339 timestamp (fractional seconds may be present), the hard server-side
expiry measured from setup admission, not from answer delivery. It is not a
call ID or a renewable lease.

Errors use the existing OpenAI-shaped JSON error envelope:

| Status | Meaning |
| --- | --- |
| 401 | Existing serve authentication rejected the request |
| 400 | Invalid JSON, fields, SDP, or content limits |
| 405 | Not POST (`Allow: POST`) |
| 413 | Encoded body exceeds the limit |
| 415 | Not application/json |
| 429 | Four concurrent setup/active sessions already admitted |
| 503 | Disabled, auth-none mode, missing executable/OAuth, or server stopping |
| 502 | App-server protocol/startup/realtime failure, including incompatible Codex |
| 504 | Setup exceeded 45 seconds |

## Exact Codex integration

The child is the configured executable invoked as:

```text
codex app-server -c cli_auth_credentials_store="ephemeral" -c history.persistence="none" -c analytics.enabled=false
```

These are literal argv entries, not shell interpolation. Each call has a
private temporary working directory and HOME/CODEX_HOME/XDG roots. The child
inherits only PATH plus those isolated roots and `RUST_LOG=off`; it does not
inherit API keys, shell startup settings, user Codex config/MCP servers, tracing
configuration, or proxy environment variables.

Newline-delimited app-server RPC runs in this order:

1. `initialize` with clientInfo and `capabilities.experimentalApi: true`, then
   `initialized` notification.
2. `account/login/start` with `type: "chatgptAuthTokens"`, `accessToken`, and
   `chatgptAccountId` from term-llm's existing native ChatGPT OAuth store. Codex
   derives the plan from the access token; no new credential store is created.
3. `thread/start` with `ephemeral: true`, isolated cwd, `approvalPolicy:
   "never"`, `sandbox: "read-only"`, empty base/developer instructions, and
   `config: {"features.realtime_conversation": true}`.
4. `thread/realtime/start` with the returned threadId, `transport:
   {"type":"webrtc","sdp":<offer>}`, `version: "v3"`, `outputModality:
   "audio"`, `clientManagedHandoffs: true`, `includeStartupContext: false`,
   `model: "gpt-live-1-codex"`, `prompt: <instructions>`, and
   `initialItems: <initial_items>`.
5. Wait for both the successful start RPC acknowledgement and matching
   `thread/realtime/sdp` notification; return its SDP answer.

There are **no hand-crafted private HTTP calls** in this bridge. Codex owns
call negotiation and the joined sideband. Its stdin remains open and stdout is
continuously drained after the HTTP response. The app handles
`delegation.created` and sends `delegation.context.append` on its own data
channel; the bridge does not launch term-llm delegation runs or forward model
transcripts. Server requests other than `account/chatgptAuthTokens/refresh` are
rejected. Refresh uses the same generation-safe native OAuth refresh/storage
path as the existing ChatGPT provider, sends only access token/account ID to
Codex, and refuses to switch accounts during a call.

RPC fields and the SDP notification path were checked against OpenAI Codex
source at `31ffe2bc9adccfe5fd3d29208250f796a13aa7a0`, notably
`codex-rs/app-server-protocol/src/protocol/v2/{account,realtime,thread}.rs`,
`request_processors/account_processor.rs`, and the
`webrtc_v3_start_posts_live_session_and_joins_without_session_update` upstream
test. This API is experimental; the external-token login is explicitly marked
unstable upstream. There is no claim of compatibility with older Codex releases.

## Lifecycle, diagnostics, and limitations

- At most **four** sessions per serve instance, including setup. One process
  per session. Hard **15-minute TTL**, including setup. Setup deadline is
  **45 seconds**.
- Request cancellation during setup cancels/kills the child. HTTP request
  completion after successful negotiation **does not** kill the sideband.
  Failed HTTP response writes cancel the negotiated session.
- Expiry, serve shutdown, app-server EOF/exit, malformed/oversized RPC frames,
  or realtime error/closed notifications cancel the child. It is reaped and
  the temporary directory removed; shutdown waits for process cleanup.
- Native OAuth refresh uses the existing provider's bounded network timeout
  and credential locks rather than introducing another token implementation.
  That API is not context-aware while inside refresh/lock acquisition: a
  canceled setup handler can take longer to unwind there, although its child
  process is canceled promptly. Ordinary serve shutdown still reaps children.
- There is no explicit DELETE, renewal, resume, sideband reconnect, app-server
  pooling, trickle-ICE endpoint, or automatic Codex installation. A client that
  disappears after the answer may hold a slot until Codex reports closure or
  the TTL expires. Calls end on server restart. Cleanup uses process
  cancellation rather than waiting for `thread/realtime/stop` acknowledgements.
- No raw child stderr, RPC frames, OAuth errors, tokens, account IDs, SDP,
  instructions, or transcripts are logged. Diagnostics expose fixed operation
  names, numeric RPC error codes, and generic lifecycle failures only. The
  temporary Codex home is not a supported durable diagnostics location.
- This is process/configuration isolation, not an OS security boundary against
  a malicious configured executable. Only use a trusted Codex installation.
- No installed Codex binary or live account was exercised for this change.
  Verification uses fake real subprocesses for the exact RPC sequence,
  notification ordering, refresh, cancellation, capacity, TTL, cleanup, and
  failure/redaction paths, plus authenticated HTTP validation tests. Real
  account/model entitlement, WebRTC media, network reachability and actual
  data-channel delegation need an end-to-end jarvis-app smoke test.
