// Package peerbroker implements the version 2 linked-device rendezvous service.
//
// A host proves possession of an Ed25519 key by signing a short-lived broker
// challenge. Successful registration creates a renewable, short (three minute
// by default) host lease and returns a lease capability. Registration and each
// renewal carry the complete set of active per-device reconnect capability
// hashes; omitting a device revokes it. The host generates every device
// reconnect capability, and the broker stores only its SHA-256 hash.
//
// A leased host may register bounded invitation slots by sending an invitation
// ID, expiry, and SHA-256 hash of a host-generated invitation capability. The
// plaintext invitation capability is never returned by the broker. It is
// accepted for one WebSocket connection only. Device reconnect capabilities
// are independently authorized by the hash set from host registration/renewal
// and are never disclosed to the host in broker peer frames.
//
// Signalling is live-only and routed over bounded WebSockets. It is not a
// mailbox: signals for an offline peer are rejected and never retained. The
// host selects a peer by peer_kind and peer_id; an invitation/device can only
// address its leased host. Payloads are opaque JSON and are bounded by size,
// queue depth, rate and lifetime count.
//
// The HTTP contract is:
//
//	POST /v2/challenges                       create host-key challenge
//	POST /v2/hosts                            verify signature/register lease
//	POST /v2/hosts/{host}/renew               renew and replace device hashes
//	POST /v2/hosts/{host}/invitations         register invitation hash/expiry
//	GET  /v2/hosts/{host}/ice[?device|invitation=id]
//	GET  /v2/hosts/{host}/ws[?device|invitation=id]
//
// Capabilities are Bearer credentials and never appear in URLs. Challenge
// creation additionally requires Config.AdmissionCredential when configured.
// Responses are Cache-Control: no-store. No public broker, STUN, TURN, DNS,
// deployment, or hosted-provider default is embedded by this package.
package peerbroker
