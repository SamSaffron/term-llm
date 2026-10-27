---
title: Linked devices
---

# Linked devices

Linked devices is a preview feature for connecting a compatible client to one
`term-llm serve web` target without exposing its HTTP listener to the internet.
Open **Settings → Linked devices** to see the target's current state.

## Availability

The tab is always honest about availability. A server needs bearer or passkey
authentication and an operator-provided rendezvous service. term-llm does not
ship a public broker URL or silently make an outbound connection. With no
rendezvous configured, the tab says **Not available** and linking remains off.
The repository includes a synthetic Go/Pion client for protocol and local
integration testing; it is not production client evidence.

## Pair a device

1. Open **Settings → Linked devices**.
2. Select **Connect**.
3. Scan the five-minute, one-use QR code or copy the pairing link into a
   compatible client.
4. Enter the six-digit confirmation code shown by that client before the pending card's
   two-minute approval countdown expires.
5. Review the authority statement and approve.

The QR secret is returned once and is not persisted. Closing Settings does not
stop the server-owned runtime, but reopening it cannot reveal the old secret;
cancel that invitation and create another. A second valid device attempting to
use the same invitation cancels it.

Approved devices survive a server or container restart and reconnect without a
new QR. Temporary broker loss is shown as **Reconnecting** and the server
re-registers with bounded backoff; **Resume** can request an immediate retry. A process
reload or server shutdown cancels active streams, so clients retry them after
reconnecting. **Stop sharing** disconnects devices but keeps approvals.
**Forget all** clears every approval and rotates this target's host
identity. Recreating a deleted container volume produces a different identity;
a client must not silently replace its pinned host key.

Revoking a device closes its active stream and refuses reconnects. Revocation
stops that device's future connections only. It cannot undo actions already
taken or retrieve information already disclosed.

## Security boundary

A paired device reaches a positive allowlist of this server's chat, session,
event, and read-only media routes over an authenticated WebRTC DataChannel. It
cannot reach linked-device owner endpoints, auth, admin, MCP, jobs, shell,
push, live voice, upload creation, transcript publishing, workspace file-content,
worktree, or extension/widget management routes. The peer capability response
hides or explains those unsupported controls. This allowlist limits network
management surfaces; it is not a tool sandbox.

The agent and sharing runtime run as the same OS user. In the standard contained
agent the agent also has passwordless sudo, runs with `--yolo`, and can read the
server's environment and state. It can therefore alter sharing state or use the
server's existing authority. File permissions and revocation protect against
other network parties, not against the agent. If that boundary is unacceptable,
stop the server from outside it, destroy the container state if appropriate,
and rotate provider credentials. An agent-proof design requires a separate host
controller and is not provided by this feature.

The broker and TURN relay see connection metadata such as IP addresses, timing,
and presence. Pairing messages and SDP fingerprints are authenticated, and
DataChannel payloads are protected by DTLS; the broker and relay cannot grant
term-llm access or read chat traffic.

## Configure rendezvous and ICE

Linked devices remains unavailable until an administrator configures a rendezvous
broker. The standard `config.yaml` surface is:

```yaml
linked_devices:
  broker_url: "https://broker.example.com"
  admission_credential: "file:///run/secrets/term-llm-broker-admission"
  diagnostics: false
  ice_servers:
    - urls: ["stun:stun.term-llm.com:3478"]
```

The default ICE policy includes term-llm's free STUN service at
`stun:stun.term-llm.com:3478`. STUN helps discover direct connectivity; it does not
relay chat traffic. TURN is **not** enabled by default: configure your own TURN
service explicitly if your networks require a relay. Loading configuration does
not contact ICE servers while linked devices is unavailable or stopped.

`broker_url` also has no default. Configure a broker you trust; term-llm does
not supply a public broker or credential issuer.
`TERM_LLM_LINKED_DEVICES_BROKER_URL` and `TERM_LLM_LINKED_DEVICES_ADMISSION`
remain deployment overrides when their YAML fields are empty. Broker URLs require HTTPS, except that loopback HTTP is
accepted for local development; credentials, query strings, and fragments are
not valid broker URLs.

`diagnostics: true` opts this serve process into a local stderr timeline prefixed
with `[linked-devices]`. Records contain only fixed lifecycle outcomes, monotonic
elapsed time, finite route templates, candidate class, status, duration, and
bounded counters. They never include URLs or queries, device/session identifiers,
credentials, headers, bodies, SDP, network addresses, or arbitrary error text.
The default is `false` and emits nothing.

`ice_servers` is a list of standard WebRTC ICE server records:

- STUN entries use `stun:` or `stuns:` URLs and have no credentials.
- TURN entries using `credential_source: broker` have no username or credential
  in YAML. The broker asks its configured server-side TURN credential provider
  for bounded credentials, and refuses to apply them to a relay URL that the
  provider does not advertise. The bundled broker currently supports coturn
  TURN-REST; the host policy itself is provider-neutral.
- TURN entries using `credential_source: static` require both `username` and
  `credential`. The credential supports term-llm's deferred secret references.
  The trusted broker receives and holds it in memory for the host lease so it
  can deliver it to the host and an approved device capability; it is never
  delivered to an invitation/QR capability.
- Invitation ICE requests are signalling-only and receive STUN entries only;
  they never consume TURN mint quota or call the TURN credential provider.
- Omitting `ice_servers` or setting it to `null` uses the free STUN-only default.
  An explicit `ice_servers: []` disables STUN and TURN for host candidates only.
  Neither policy falls back to broker-wide TURN settings or mints credentials.

For example, explicitly configure your own STUN server and a private static TURN
account without placing its password directly in YAML:

```yaml
linked_devices:
  broker_url: "https://broker.example.com"
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
    - urls:
        - turn:turn.example.com:3478?transport=udp
      credential_source: static
      username: "term-llm"
      credential: "file:///run/secrets/term-llm-turn-password"
```

Changing the broker URL requires **Forget all** because clients pin the broker
and host identity. Restart the serve process after changing the local ICE policy.
If registration was rejected because the broker's TURN provider did not match
the configured policy, fix the broker and use **Resume** to make one deliberate
new registration attempt; the runtime does not retry permanent policy errors in
the background. Existing connections continue until they disconnect; new
connections fetch the current policy and fresh broker credentials. Any live QR
invitation is cancelled with a warning after a broker reconnect because the new
broker lease cannot retain the old invitation slot.

## Self-hosted broker and TURN credentials

`cmd/peer-broker` is the standalone rendezvous service. It binds to loopback by
default and requires TLS when bound to a non-loopback address. For example,
keep it behind an HTTPS reverse proxy with WebSocket support and configure your
own coturn TURN-REST provider:

```text
peer-broker \
  --host 127.0.0.1 --port 8091 \
  --admission-credential-file /run/secrets/term-llm-broker-admission \
  --stun stun:stun.example.com:3478 \
  --turn 'turns:turn.example.com:5349?transport=tcp' \
  --turn-rest-secret-file /run/secrets/term-llm-turn-rest \
  --turn-username term-llm \
  --turn-ttl 1h
```

The admission credential and TURN-REST secret are different secrets. Keep both
private. The REST secret must match your coturn configuration; it is never
returned by the broker or placed in the host's `config.yaml`. Configure the host
explicitly to request credentials for that provider:

```yaml
linked_devices:
  broker_url: "https://broker.example.com"
  admission_credential: "file:///run/secrets/term-llm-broker-admission"
  ice_servers:
    - urls: ["stun:stun.example.com:3478"]
    - urls: ["turns:turn.example.com:5349?transport=tcp"]
      credential_source: broker
```

A broker provider does not opt hosts into using TURN: each host must configure
its own ICE policy. Invitations receive only the STUN entries from that policy,
never TURN credentials. Operate your relay with authentication, bounded
credentials, allocation quotas, and egress monitoring; never enable anonymous
TURN or an unrestricted credential issuer. Deployment-specific DNS, TLS,
firewall, relay ranges, and service management belong to the relay operator.

To return to host-only connectivity, set `linked_devices.ice_servers: []` and
restart serve.

The complete pairing and broker contracts are documented in
`internal/pairing/WIRE.md` and `internal/peerbroker/WIRE.md` in the source tree.
