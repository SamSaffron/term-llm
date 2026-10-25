# term-llm pairing wire format v2 (FROZEN)

Pairing v2 introduces identities and revocation-only grants. It carries no SDP.
WebRTC SDP appears only in the later signed transport handshake. Binary values
use canonical unpadded base64url. Keys are Ed25519 (32-byte public keys,
64-byte signatures); integer times are Unix seconds.

`WireVersion == 2` is independent of the private `StateVersion == 1`. State
files are never wire messages.

## 1. Pairing link

```text
termllm://pair?v=2&b=<escaped URL>&h=<b64u host key 32B>&i=<b64u invitation id 16B>&e=<unix>[&n=<escaped host label>]#s=<b64u pairing secret 32B>[&c=<invitation capability>]
```

The query names are exactly `v`, `b`, `h`, `i`, `e`, and optional `n`.
The fragment names are exactly `s` and optional `c`. `s` and `c` are bearer
secrets and MUST NOT appear in the query or logs; use `Link.Redacted`.

Parsing rejects unknown, duplicate, empty, misplaced, non-canonical or wrongly
sized parameters; paths, userinfo, and fragments/queries inside the broker URL;
and every scheme/authority except exact `termllm://pair`. Emitters use the order
shown, but parsers do not depend on order.

## 2. Transcripts and SAS

```text
field(x)   = uint32_big_endian(len(x)) || x
transcript = field("term-llm/pairing/v2") || field(label) || field(f1) || ...
```

Strings are UTF-8, binary fields are raw bytes, and integer fields are canonical
decimal ASCII.

| label | fields in order |
|---|---|
| `pair-request` | `"device"`, invitation_id, host_key, device_key, device_nonce, device_name |
| `pair-answer` | `"host"`, invitation_id, host_key, device_key, device_nonce, request_hash, device_id, reconnect_capability, scope, base_path, host_label |
| `pair-sas` | invitation_id, host_key, device_key |
| `transport-hello` | `"device"`, host_key, device_key, device_id, reconnect_capability, device_nonce |
| `transport-challenge` | `"host"`, challenge_id, host_key, host_nonce, device_key, device_id, expires_at |
| `transport-request` | `"device"`, challenge_id, host_key, host_nonce, device_key, device_nonce, device_id, offer_sdp |
| `transport-answer` | `"host"`, challenge_id, host_key, host_nonce, device_key, device_nonce, request_hash, device_id, answer_sdp |

`request_hash = SHA-256(request transcript)`.

```text
mac_key = HMAC-SHA256(key=s, msg="term-llm/pairing/v2/mac-key")
mac     = HMAC-SHA256(key=mac_key, msg=transcript)
sas_mac = HMAC-SHA256(key=mac_key, msg=pair-sas transcript)
sas     = zero-padded decimal(uint32_be(sas_mac[0:4]) mod 1000000)
```

## 3. Identity-only pairing

Device to host:

```json
{"v":2,"type":"pair_request","invitation_id":"...","device_key":"...","device_nonce":"...","device_name":"phone","mac":"...","sig":"..."}
```

The host validates shape, locates the invitation, and checks MAC before
revealing duplicate/claimed state. A bad MAC does not consume an invitation.
After a valid MAC, a duplicate, bad binding/signature, denial, expiry,
cancellation, or successful completion burns the invitation. A valid first
request claims it while explicit approval is pending. Both sides derive the
six-digit SAS only for an authenticated request.

Host to device after approval and durable persistence:

```json
{"v":2,"type":"pair_answer","invitation_id":"...","host_key":"...","device_key":"...","device_nonce":"...","request_hash":"...","device_id":"...","reconnect_capability":"...","scope":"web-v1","base_path":"/","host_label":"...","mac":"...","sig":"..."}
```

`device_id` is a host-assigned random 16-byte identifier.
`reconnect_capability` is a random 32-byte bearer secret returned exactly once.
The host persists only SHA-256(reconnect_capability), never the raw capability.
The grant records device identity/name, `web-v1` scope, base path, paired time,
last-seen time, and optional revoked time. It does not expire: it remains valid
until explicitly revoked or pairing state is reset. The client stores the raw
capability as secret material. `pair_reject` is advisory and unauthenticated.

## 4. Signed transport handshake

1. Device sends signed `transport_hello` with its device ID, raw reconnect
   capability, identity key, and a fresh nonce.
2. Host checks signature, capability hash, and current revocation state, then
   returns a signed, short-lived, one-use `transport_challenge`.
3. Device checks the pinned host signature and sends a signed
   `transport_request` binding the exact offer SDP bytes.
4. Host burns every recognized challenge before checking bindings, signature,
   and grant state, updates last-seen, and returns a signed `transport_answer`.
5. Device checks the pinned key, echoes, request hash, signature, and SDP
   fingerprint before applying the answer.

Revocation is re-read at challenge, request, and answer time. SDP is at most
64 KiB, is transcript-bound byte-for-byte, and must contain consistent valid
DTLS fingerprint lines.

## 5. Deterministic vectors

`testdata/vectors_v2.json` fixes keys, nonces, pairing messages, transcripts,
MAC key, MAC, signatures, request hash, and SAS. Implementations must reproduce
the values byte-for-byte.
