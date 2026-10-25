---
title: Mobile sharing
---

# Mobile sharing

Mobile sharing is a preview feature for connecting a native mobile client to one
`term-llm serve web` target without exposing its HTTP listener to the internet.
Open **Settings → Mobile Sharing** to see the target's current state.

## Availability

The tab is always honest about availability. A server needs bearer or passkey
authentication and an operator-provided rendezvous service. term-llm does not
ship a public broker URL or silently make an outbound connection. With no
rendezvous configured, the tab says **Not available** and sharing remains off.

The native iPhone companion and any public broker/TURN deployment are separate
from the public core implementation. The repository includes a synthetic Go/Pion
test device for protocol and local integration testing; it is not a phone.

## Pair a device

1. Open **Settings → Mobile Sharing**.
2. Select **Connect a phone**.
3. Scan the five-minute, one-use QR code with a compatible native client.
4. Enter the six-digit code shown by that client in the pending request card.
5. Review the authority statement and approve.

The QR secret is returned once and is not persisted. Closing Settings does not
stop the server-owned runtime, but reopening it cannot reveal the old secret;
cancel that invitation and create another. A second valid device attempting to
use the same invitation cancels it.

Approved devices survive a server or container restart and reconnect without a
new QR. **Stop mobile sharing** disconnects devices but keeps approvals.
**Forget all phones** clears every approval and rotates this target's host
identity. Recreating a deleted container volume produces a different identity;
a client must not silently replace its pinned host key.

Revoking a device closes its active stream and refuses reconnects. Revocation
stops future mobile-sharing connections only. It cannot undo actions already
taken or retrieve information already disclosed.

## Security boundary

A paired device reaches a positive allowlist of this server's chat, session,
event, and read-only media routes over an authenticated WebRTC DataChannel. It
cannot reach mobile-sharing owner endpoints, auth, admin, MCP, jobs, shell,
push, live voice, uploads, publishing, worktree, or extension/widget management
routes. This allowlist limits network management surfaces; it is not a tool
sandbox.

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

## Operator-only local setup

`cmd/peer-broker` is a standalone broker for development and owned deployments.
It binds to loopback by default and requires TLS off loopback. It accepts static
STUN URLs and coturn TURN-REST settings; it has no public defaults. The serve
runtime reads `TERM_LLM_MOBILE_SHARING_BROKER_URL` and the optional
`TERM_LLM_MOBILE_SHARING_ADMISSION` from its environment. These are internal
operator injection points, not ordinary serve flags or YAML settings.

The complete pairing and broker contracts are documented in
`internal/pairing/WIRE.md` and `internal/peerbroker/WIRE.md` in the source tree.
