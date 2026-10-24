// Package pairing implements secure device pairing primitives: QR links, persistent identities, transcript MACs/signatures, approval and grants.
//
// The wire format is frozen as v1 and documented in WIRE.md. Host side:
// NewHost, Host.NewPairingForSession, Host.VerifyPairRequest, Host.Approve,
// Host.CompletePairing, and Host.NewChallenge/VerifyReconnect/AnswerReconnect
// for reconnects. Device side: StartPairing and StartReconnect.
package pairing
