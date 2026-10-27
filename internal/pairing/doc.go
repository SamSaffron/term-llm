// Package pairing implements the frozen v2 identity-pairing and signed
// transport-handshake contract, strict termllm://pair invitations, persistent
// Ed25519 identities, and durable revocation-only web-v1 grants.
//
// WIRE.md specifies every transcript, link field, SAS derivation, and state
// boundary. Pairing is identity-only; SDP appears only in transport messages.
package pairing
