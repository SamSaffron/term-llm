---
title: "Peer connect pilot"
weight: 17
description: "Experimental authenticated WebRTC access to a local contain Hub."
kicker: "Deploy agents"
---

This is an **experimental foreground pilot**, not a deployed pairing service or
an App Store installation workflow. Native SDK, ATS and WebKit verification must
pass before claiming iOS support. The native app uses its native Hub screens and
a capability-authenticated localhost HTTP bridge for the existing chat UI.

## Scope and lifetime

`contain pair` serves existing contain workspaces through a local Hub. It does
not create containers or install/deploy a broker. A separately approved HTTPS
broker handles authenticated signalling and optional short-lived TURN minting;
Hub/chat payloads travel over an end-to-end DTLS-encrypted WebRTC DataChannel.

A one-use native pairing link contains a fresh 256-bit PSK and an independent
device signalling capability **in its fragment**, never a Hub master token. Keep
the link out of logs, transcripts and recordings. The terminal operator reviews
and explicitly approves the device identity. HMAC and identity signatures bind
the complete SDP, including the DTLS fingerprint, so a malicious signalling
broker cannot substitute another peer. The broker can still observe signalling
metadata and deny service.

The approved scope is **Hub browsing and agent chat on all discovered nodes**.
Hub passkey/security management, node registration and registration credentials
are excluded. Security route denial is separate from grant revocation. The Hub
HTTP listener retains its existing authentication; the peer ingress is never
mounted as an unauthenticated HTTP route.

Grants expire after 24 hours by default. Host keys and grants persist in private
files, but **pilot reconnect discovery only works through the original live
broker session, at most 30 minutes**. App reconnect within that live rendezvous
uses a fresh signed challenge. Host restart or broker-session expiry requires
new pairing; this is not restart-persistent discovery. Existing connected peers
may continue when signalling expires, subject to grant expiry/revocation.

## Operator commands (code only; deployment requires separate approval)

On an approved broker host, with a private registration capability file:

```sh
term-llm serve peer-broker \
  --registration-token-file /private/pilot-registration-capability
```

Default binding is `127.0.0.1:8091`, intended for an approved HTTPS reverse proxy.
Non-loopback binding requires both `--tls-cert` and `--tls-key`. Session creation
fails closed without the private pilot registration capability. Defaults bound
sessions, connections, message size/count/queues, rate and lifetime. No HTTP/chat
relay endpoint exists.

Optional Cloudflare minting requires both `--cloudflare-turn-key-id` and
`--cloudflare-turn-api-token-file`; the API token stays on the broker. Native and
host peers receive only short-lived credentials. A mocked mint API test is not
proof of Cloudflare relay operation.

On the contain host, after starting the desired real agent workspaces:

```sh
term-llm contain pair \
  --broker https://approved-broker.example \
  --registration-token-file /private/pilot-registration-capability
```

This is foreground and requires an interactive terminal. Open the secret link
in the native app and type `yes` only after reviewing the verified device key
and scope. STUN defaults to `stun:stun.cloudflare.com:3478`; override `--stun` for
an approved alternative. Add `--turn` to request short-lived broker credentials;
mint failure is an error, not silently successful fallback. `--relay-only`
requires `--turn` and restricts ICE for diagnostics; **that option alone is not
proof that traffic used a relay**.

List grants with `term-llm contain peers`, and revoke one with
`term-llm contain peers revoke <grant-id>`. For a non-default directory use
`--state-dir` on these commands. New requests fail immediately after revocation;
the active peer closes within one second.

Identity and grants default to the private `peer` directory beside the Hub node
store. `--peer-state-dir` selects another private directory. The local Hub binds
loopback and does not print its generated master token in pair mode.

## Transport limits and exclusions

The v1 `term-llm-http-v1` channel carries bounded HTTP requests and incremental
response frames, including SSE, with cancellation and backpressure. Request
bodies must fit one 64 KiB encoded frame (roughly 47 KiB raw); larger uploads are
not supported. Generic WebSocket upgrades, widgets requiring them, original-RP
WebAuthn ceremonies through localhost, and voice are not claimed in this pilot.

## Evidence and outstanding gates

Core tests exercise two actual Pion peers, authenticated broker WebSockets,
identity-bound SDP, the real Hub proxy and real serve engine APIs. The integration
model is explicitly a deterministic test provider, not a paid remote provider.
A separate in-process **local test TURN** verifies nominated relay candidates,
application bytes and relay socket traffic. That is not Cloudflare evidence.

Full acceptance still needs the pinned native SDK compiled on macOS, actual
localhost ATS/header/SSE/WebKit behavior, an owned remote Docker backend, and an
actual simulator recording with redacted correlated request/ICE statistics.
Cloudflare forced-relay proof is blocked until Cloudflare credentials are
available. No fixture chat, static screenshots or fake App Store installation
can substitute for these gates.
