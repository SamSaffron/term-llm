# Peer broker wire contract v2

This document defines the device-facing rendezvous contract used with
`internal/pairing/WIRE.md`. The broker is a signalling relay. It does not grant
term-llm authority and it cannot authenticate payload contents.

## Stable identifiers and URLs

`host_id = base64url(SHA-256(host_ed25519_public_key))`.

A device uses these authenticated endpoints:

```text
GET /v2/hosts/{host_id}/ice?invitation={invitation_id}
GET /v2/hosts/{host_id}/ice?device={device_id}
GET /v2/hosts/{host_id}/ws?invitation={invitation_id}
GET /v2/hosts/{host_id}/ws?device={device_id}
```

`invitation_id` and `device_id` are canonical unpadded base64url encodings of
16 random bytes. The invitation request presents the fragment `c` capability
from the pairing link. A paired-device request presents the 32-byte reconnect
capability from `pair_answer`, encoded as canonical unpadded base64url.

HTTP and WebSocket upgrades accept `Authorization: Bearer <capability>`. The
stable browser-compatible alternative is to offer both WebSocket subprotocols:

```text
peerbroker.v2
peerbroker.v2.bearer.<capability>
```

The server selects and echoes only `peerbroker.v2`, never the credential-bearing
protocol. Native clients SHOULD use the Authorization header; they MAY fall
back to the subprotocol form when a WebSocket stack or proxy cannot set it.
Broker URLs require HTTPS/WSS except on loopback.

## Grant publication and pairing sequence

1. The device uses the one-use invitation socket for identity-only
   `pair_request` / `pair_answer`.
2. On approval the host persists only `SHA-256(raw reconnect capability)`.
3. **Before sending `pair_answer`, the host waits for a successful lease renewal
   that atomically publishes the new `{device_id, capability_hash}` set.** The
   raw reconnect capability is sent only inside authenticated `pair_answer`.
4. The device closes the invitation socket. It may immediately request device
   ICE and open `?device=<device_id>`; a bounded retry for transient transport
   failure is still recommended, but `unauthorized` is not normal propagation.
5. Every later connection uses the device endpoint and the signed transport
   hello/challenge/offer/answer from pairing WIRE v2. No QR is reused.

Renewal replaces the complete device hash set. Removing or changing an entry
revokes that broker credential and closes its active socket. The validated host
ICE policy remains attached to that lease; process restart/re-registration sends
and signs it again. Invitations are one-use at the broker and authenticated
host. A second socket presenting the same invitation capability invalidates the
broker slot and closes both sockets; it never replaces the original answer
route. A disconnected consumed invitation cannot reconnect. The host may also
revoke an unused slot early with authenticated
`DELETE /v2/hosts/{host}/invitations/{invitation}`. The broker cannot
authenticate a pairing request itself. Broker slots remain memory-only and
expire at their advertised expiry. Host leases default to three minutes and are
renewed every minute.

## Host ICE policy

Host registration may include an ordered `ice_policy` array. The exact array is
included in the Ed25519 registration transcript after the canonical device hash
set. Omitting the field preserves the broker's legacy global STUN/coturn policy;
an explicit empty array selects host candidates only.

Each entry contains `urls` plus one credential mode:

- STUN/STUNS has no `credential_source`, username, or credential;
- `credential_source: "broker"` is TURN/TURNS without embedded credentials. The
  broker accepts only URLs advertised by its configured server-side credential
  provider and returns bounded principal credentials;
- `credential_source: "static"` is TURN/TURNS with a host-supplied username and
  credential. The broker holds these credentials in memory for the trusted host
  lease and returns them only to the host and an authorized device capability.

Invitation ICE requests return only STUN entries from either an explicit policy
or the legacy global configuration. They never receive static or broker-minted
TURN credentials, call the TURN credential provider, or consume mint quota.

The host and approved device independently call the authenticated ICE endpoint
for every new transport, so both consume the same accepted policy and receive
fresh broker credentials. Static/STUN-only responses do not consume mint quota.
A broker REST shared secret is deployment-private and never appears in this
wire contract.

## WebSocket envelopes

All messages are JSON text frames.

```json
{"type":"ready","role":"device","peer_kind":"device","peer_id":"...","peer_connected":true,"lease_expires_at":"..."}
{"type":"signal","peer_kind":"host","body":{"v":2,"type":"transport_hello"}}
{"type":"delivered","peer_kind":"invitation","peer_id":"..."}
{"type":"peer","peer_kind":"host","peer_connected":false}
{"type":"error","peer_kind":"host","error":"peer_offline"}
```

Device and invitation senders MUST omit routing fields on `signal`; the broker
sets their authenticated source identity. The host supplies `peer_kind` and
`peer_id` when routing to a device or invitation. A host-to-invitation signal
receives `delivered` only after the broker has queued it to that exact live
invitation slot; this is broker route-queue acceptance, **not client receipt**.
A later network loss can therefore leave an accepted grant undelivered. A
definitive routing failure instead returns `error`, allowing reconnect-capability
publication to roll back. `body` is exactly one pairing v2 JSON message. There
is no offline mailbox.

`ready`, `peer`, `delivered`, and `error` are **unsigned broker advice**. They are useful for
retry/UI state but convey no host authority. `pair_request`, `pair_answer`, and
all transport messages have the MAC/signature authority defined by pairing
WIRE v2. `pair_reject` is also advisory and unsigned. ICE server data is
transport-authenticated by broker HTTPS and authorized by the presented
capability, but is not host-signed. The signed SDP fingerprints bind the later
DTLS/DataChannel to the pinned host/device identities.

## Errors and device UX

HTTP errors use `{ "error": "<code>", "message": "..." }` and
`Cache-Control: no-store`.

- `unauthorized` on a previously paired device endpoint: the device credential
  is absent, changed, or revoked. Show **access revoked or target reset**, not
  merely offline. A bad local credential is intentionally indistinguishable.
- `not_found`: the host identity is not registered at this broker. Show offline;
  after repeated registration cycles, offer identity-reset troubleshooting.
- `expired`, or WebSocket close `4002 expired`: the host lease or invitation
  expired. Retry with backoff; this is not proof of revocation by itself.
- `peer_offline`: the lease exists but the host WebSocket is not attached. Show
  offline and retry. This is distinct from `unauthorized`.
- close `4001 superseded`: a newer socket for this same device identity replaced
  this one. Stop the old connection and let the newest connection own it.
- close `4003 broker_closed`: service shutdown; retry with backoff.
- close `4004 rate_limited`, `4005 quota_exceeded`, `4006 bad_request`, or
  standard `1009 too_large`: stop the attempt and surface a bounded error.

A host identity reset changes `host_id`. The broker cannot sign an
"identity changed" assertion. The app's authority is its pinned host key: a
message signed by a different key is identity changed; failure to find the old
`host_id` is only advisory/offline evidence and MUST NOT silently replace the
pin.

## Default limits

- signal `body`: 256 KiB; complete WebSocket envelope: body plus 512 bytes;
- pairing decoder: 256 KiB; SDP field: 64 KiB;
- pending outbound frames per socket: 32;
- signals: 20/second, burst 40;
- signals per socket: 512; per host lease: 4096;
- active WebSockets globally: 512;
- reconnect hashes per broker host: 32 (the term-llm runtime admits at most 16
  paired devices and four concurrent device sessions);
- invitations per host: 16;
- ICE mints per authenticated principal per lease: 8 when TURN credentials are
  minted; static STUN-only responses do not consume this quota. Renewal resets
  each principal's bounded counter.

Clients MUST enforce the 256 KiB decoded pairing-message limit before parsing
and SHOULD cap the outer broker frame at 256 KiB + 512 bytes. Limits are server
configuration ceilings; a deployment may choose lower quotas and report a
rate/quota error.
