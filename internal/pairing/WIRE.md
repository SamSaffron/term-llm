# term-llm pairing wire format v1 (FROZEN)

All binary values in links and JSON are **base64url without padding** (RFC 4648 §5).
Keys are Ed25519 (32-byte public keys, 64-byte signatures). Integers are Unix
seconds. SDP strings are carried and signed **byte-for-byte exactly as sent**; a
receiver must hand the exact signed string to WebRTC (never re-serialise before
verifying).

## 1. Pairing link (QR)

```
term-llm-pair://v1?broker=<url-escaped broker URL>&sid=<b64u 16B>&hk=<b64u 32B>&hn=<b64u 32B>&exp=<unix>[&name=<url-escaped host label>]#ps=<b64u 32B>[&cap=<broker device capability>]
```

Query (non-secret):

| param | meaning |
|---|---|
| `broker` | broker base URL (https; plain URL, no userinfo/query/fragment) |
| `sid` | broker session ID **exactly as issued by the broker** (= b64u of 16 random bytes); transcripts use the 16 decoded bytes |
| `hk` | host Ed25519 public key (pin it) |
| `hn` | host nonce for this initial connection (random, 32 bytes) |
| `exp` | link expiry (Unix seconds, canonical decimal); never later than the broker session expiry |
| `name` | optional host label (same rules as `device_name`) |

Fragment (bearer material; never in the query, never logged — log `Link.Redacted()`):

| param | meaning |
|---|---|
| `ps` | pairing secret (crypto/rand, 32 bytes). **Never** sent to the broker or anywhere else; not the master token |
| `cap` | optional broker device signalling capability (1–128 chars of base64url alphabet). Independent of `ps`, never PSK-derived, and **not** part of any transcript |

Parsing is strict: query and fragment are `k=v` pairs joined by `&`; unknown,
duplicate, empty or misplaced parameters (e.g. `ps`/`cap` in the query) are
rejected, as are non-canonical base64url, wrong lengths, paths, userinfo and any
version other than `v1`. The emitted order is the order shown above, but
parsers must not depend on order.

## 2. Transcript bytes (what gets MACed/signed)

```
field(x)   = uint32_big_endian(len(x)) || x
transcript = field("term-llm/pairing/v1") || field(label) || field(f1) || field(f2) || ...
```

Strings are UTF-8; byte fields are raw bytes (not base64); integers are decimal
ASCII (canonical; `grant_expires_at` must be > 0). Field order per label:

| label | fields, in order |
|---|---|
| `pair-request` | `"device"`, sid, hk, hn, device_key, device_nonce, device_name, offer_sdp |
| `pair-answer` | `"host"`, sid, hk, hn, device_key, device_nonce, request_hash, grant_id, grant_expires_at, answer_sdp |
| `reconnect-request` | `"device"`, challenge_id, hk, hn, device_key, device_nonce, grant_id, offer_sdp |
| `reconnect-answer` | `"host"`, challenge_id, hk, hn, device_key, device_nonce, request_hash, grant_id, answer_sdp |

`request_hash = SHA-256(transcript of the request being answered)`.

* `mac_key = HMAC-SHA256(key = ps, msg = "term-llm/pairing/v1/mac-key")`
* `mac = HMAC-SHA256(key = mac_key, msg = transcript)` (pair messages only)
* `sig = Ed25519.Sign(sender private key, transcript)`

## 3. Initial pairing messages (JSON via broker mailbox `sid`)

Device → host:
```json
{"v":1,"type":"pair_request","sid":"..","hn":"..","device_key":"..","device_nonce":"<b64u 32B fresh>",
 "device_name":"<=64 bytes UTF-8","offer_sdp":"v=0...","mac":"<b64u 32B>","sig":"<b64u 64B>"}
```
Host checks: session exists, not expired, not consumed; `hn` matches; MAC (constant time) and
device signature over the `pair-request` transcript; SDP has exactly one consistent
`a=fingerprint:sha-256 …` value. Then the host **asks the terminal user** to approve
(showing device name + device key fingerprint). Only on approval is the session
atomically consumed and a grant persisted.

Host → device:
```json
{"v":1,"type":"pair_answer","sid":"..","hk":"..","hn":"..","device_key":"..","device_nonce":"..",
 "request_hash":"<b64u 32B>","grant_id":"<b64u 16B>","grant_expires_at":<unix, >0>,"answer_sdp":"v=0...",
 "mac":"<b64u 32B>","sig":"<b64u 64B>"}
```
Device checks `hk` equals the QR `hk`, echoes match its request, `request_hash` matches,
MAC and host signature verify — **only then** does it call setRemoteDescription(answer).
Device then stores `{hk, grant_id, device key pair}`.

Rejection (advisory, unauthenticated — treat as failure only):
`{"v":1,"type":"pair_reject","sid":"..","reason":"denied|expired|invalid"}`

## 4. Reconnect (no pairing secret; signatures only)

1. Device → host: `{"v":1,"type":"reconnect_hello","device_key":"..","grant_id":".."}`
   — sent through the original pairing mailbox (pilot v1); the host rejects any other or expired mailbox.
2. Host → device: `{"v":1,"type":"reconnect_challenge","challenge_id":"<b64u 16B>","hk":"..","hn":"<b64u 32B fresh>","expires_at":<unix>}`
   (one-use, short-lived; host keeps it in memory)
3. Device → host:
   `{"v":1,"type":"reconnect_request","challenge_id":"..","hn":"..","device_key":"..","device_nonce":"<b64u 32B fresh>","grant_id":"..","offer_sdp":"..","sig":".."}`
   Host checks challenge outstanding/unexpired/unconsumed (consumed atomically,
   success or failure), grant exists, matches device_key, not expired/revoked; signature.
4. Host → device:
   `{"v":1,"type":"reconnect_answer","challenge_id":"..","hk":"..","hn":"..","device_key":"..","device_nonce":"..","request_hash":"..","grant_id":"..","answer_sdp":"..","sig":".."}`
   Device checks `hk` equals pinned key, echoes, request_hash, signature, then installs answer.

## 5. Test vectors

`testdata/vectors_v1.json` holds fixed seeds/nonces with the expected transcript,
MAC key, MAC, signature, request hash and JSON for `pair-request`, `pair-answer`
and `reconnect-request`. Ed25519 signatures are deterministic, so native
implementations must reproduce every value byte-for-byte.

## 6. Notes

* Lifetimes (pilot v1): broker sessions live at most 30 minutes, so a pairing link
  and its approval window never outlive the broker session. Grants are persisted
  host-side with a finite expiry (default 24h; non-expiring grants are rejected,
  and a device rejects `grant_expires_at <= 0`) and fixed scope `hub-chat`.
  **Reconnect is only available through the original pairing mailbox while that
  broker session is live**; each reconnect still needs a fresh one-use host
  challenge (default 60s, capped at mailbox expiry). After the mailbox expires —
  including after an app or host restart — the persisted identities and grant
  remain valid but there is no discovery path: the host fails closed with
  `ErrReconnectUnavailable` and the device must pair again. An authenticated
  stable host lookup is future work and not part of v1.
* Grant scope `hub-chat` covers only peer access to the fixed Hub chat target. It
  never confers Hub security settings, node-registration credentials or master
  tokens; scope is host-local and not on the wire.

* `broker` may be `http://` only for local development; production links use `https://`.
* `device_name` must not contain control or Unicode format characters (it is shown in the
  host's terminal approval prompt).
* SDP must contain at least one `a=fingerprint:` line (sha-256/384/512, colon hex) and all
  fingerprint lines must agree; the transcript binds the whole SDP, so any fingerprint
  replacement breaks the MAC and signature.
