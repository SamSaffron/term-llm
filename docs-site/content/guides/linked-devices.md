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
  ice_servers:
    - urls:
        - stun:stun.term-llm.com:3478
    - urls:
        - turns:stun.term-llm.com:5349?transport=tcp
      credential_source: broker
```

The ICE defaults shown above are part of the configuration schema. Loading them
does not make a network request while linked devices is unavailable or stopped.
The owned endpoints are intended for admitted testing, not a public relay
service. `broker_url` deliberately has no public default and no public broker or
credential issuer is deployed. `TERM_LLM_LINKED_DEVICES_BROKER_URL` and
`TERM_LLM_LINKED_DEVICES_ADMISSION` remain deployment overrides when their YAML
fields are empty. Broker URLs require HTTPS, except that loopback HTTP is
accepted for local development; credentials, query strings, and fragments are
not valid broker URLs.

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
- An explicit `ice_servers: []` disables STUN and TURN and uses host candidates
  only. This is useful for same-network testing, not general internet access.

For example, a privately operated static TURN account can be configured without
placing its password directly in YAML:

```yaml
linked_devices:
  broker_url: "https://broker.example.com"
  ice_servers:
    - urls:
        - turn:relay.example.com:3478?transport=udp
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

## Testing service

The owned STUN/TURN defaults are for admitted testing. They do not provide a
public linked-devices service or an unrestricted credential issuer. TURN
requires a broker configured with the matching private REST secret, or an
administrator-provided short-lived static credential. For independent
deployments, configure your own ICE servers and admission policy.

## Operate a broker and coturn

`cmd/peer-broker` is the standalone rendezvous service for development and owned
deployments. It binds to loopback by default and requires TLS when bound to a
non-loopback address. A typical deployment keeps it on loopback behind an HTTPS
reverse proxy with WebSocket support:

```text
peer-broker \
  --host 127.0.0.1 --port 8091 \
  --admission-credential-file /run/secrets/term-llm-broker-admission \
  --stun stun:stun.term-llm.com:3478 \
  --turn 'turns:stun.term-llm.com:5349?transport=tcp' \
  --turn-rest-secret-file /run/secrets/term-llm-turn-rest \
  --turn-username term-llm \
  --turn-ttl 1h
```

The admission file and coturn REST-secret file are different secrets. Both must
be private, non-empty, and readable only by their respective services. The REST
secret must match coturn's `static-auth-secret`; it is never placed in
`config.yaml`, returned by the broker, or embedded in a TURN URL. Public broker
registration should require admission. Host-key proof prevents lease spoofing,
but it is not a substitute for operator admission or abuse controls.

For the owned `stun.term-llm.com` layout, the intended network shape is:

- a DNS-only A/AAAA record, not a Cloudflare orange-cloud proxy;
- existing STUN on UDP and TCP 3478;
- a dedicated TLS coturn listener on TCP 5349;
- a bounded, non-overlapping UDP relay range such as 40000-40127;
- firewall rules limited to those listener ports and that relay range; and
- an independently issued certificate for `stun.term-llm.com` whose renewal
  hook reloads only the dedicated relay service.

A matching dedicated coturn instance needs the equivalent of these settings
(the concrete IP addresses, service user, and secret file integration are
operator-owned):

```text
tls-listening-port=5349
realm=stun.term-llm.com
use-auth-secret
static-auth-secret=<same private value used by peer-broker>
cert=/etc/letsencrypt/live/stun.term-llm.com/fullchain.pem
pkey=/etc/letsencrypt/live/stun.term-llm.com/privkey.pem
min-port=40000
max-port=40127
no-cli
```

Do not copy the placeholder secret literally or put the real value in source,
unit files, shell history, screenshots, or logs. Configure explicit listening
and relay addresses appropriate to the host. Do not alter an existing coturn
instance merely to make the dedicated service appear deployed; use a separate
service/configuration and non-overlapping relay range.

TURN is bandwidth-bearing infrastructure. Set coturn allocation/user quotas,
monitor allocations, egress, authentication failures, and certificate expiry,
and size the broker's host/connection/ICE-mint limits for the admitted user
population. STUN can be public without making TURN an open relay. Never enable
anonymous TURN or expose an unrestricted credential issuer as a workaround for
admission policy.

## Verify and roll back

For a new deployment—or after DNS, certificate, firewall, coturn, or broker
changes—verify from a network outside the relay host:

1. DNS resolves directly to the intended host and is not proxied.
2. UDP/TCP 3478 answers STUN requests.
3. TLS on 5349 presents a valid `stun.term-llm.com` certificate.
4. An admitted broker registration can mint a bounded credential whose username
   expires as configured, while an arbitrary relay URL is rejected.
5. A forced-relay linked-device test selects relay candidates on both Pion peers
   and transfers actual response bytes.
6. An invitation capability receives STUN only and cannot retrieve or mint TURN
   credentials, while the approved device can.
7. Certificate renewal reloads only the dedicated relay service, and allocation
   and egress monitoring are visible to the operator.

To roll back, first remove TURN from `linked_devices.ice_servers` (or set the
list to `[]` for host-only mode), restart serve, and verify direct/STUN behavior.
Then stop the dedicated relay service and remove only its 5349/relay-range
firewall rules and DNS record. Do not restart or rewrite an unrelated coturn,
nginx, voice, mail, or push service during rollback.

The complete pairing and broker contracts are documented in
`internal/pairing/WIRE.md` and `internal/peerbroker/WIRE.md` in the source tree.
