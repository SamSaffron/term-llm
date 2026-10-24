// Package peerbroker implements a minimal peer signalling broker: short-lived opaque mailbox rendezvous with independent host/device bearer capabilities, bounded long-poll delivery, and optional TURN credential minting (including a Cloudflare Realtime adapter).
//
// The broker never sees pairing secrets or decrypted traffic. A host creates a
// session and receives two independent, high-entropy bearer capabilities: one
// for itself and one to hand to the device (typically inside a QR code along
// with the session ID and broker URL). Each capability may only send to the
// other side's mailbox and read its own. Message bodies are opaque JSON values
// that callers are expected to encrypt end-to-end; the broker only stores and
// forwards them, bounded by size, count, time and rate limits.
//
// Session creation requires a private registration capability
// (Config.CreateToken); NewHandler fails closed without one unless
// Config.AllowUnauthenticatedCreate is set for tests. Session IDs are
// unpadded base64url of 16 random bytes, identical to the pairing link `sid`.
//
// HTTP API (all JSON; every call requires "Authorization: Bearer <cap>",
// the registration cap for create and a session capability otherwise):
//
//	POST   /v1/sessions                       create {"ttl_seconds":N}
//	GET    /v1/sessions/{id}/ws               WebSocket registration/signalling
//	POST   /v1/sessions/{id}/messages         send {"body": <json>} to the peer
//	GET    /v1/sessions/{id}/messages?after=N&wait=S
//	                                          ack seq<=N, long-poll own mailbox
//	DELETE /v1/sessions/{id}                  close the session for both sides
//	POST   /v1/sessions/{id}/turn             mint short-lived ICE servers
package peerbroker
