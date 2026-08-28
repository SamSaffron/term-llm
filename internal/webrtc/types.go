// Package webrtc provides a WebRTC home peer for direct browser-to-server
// communication, bypassing intermediate relay servers.
package webrtc

import "time"

// Config holds configuration for the WebRTC home peer.
type Config struct {
	// SignalingURL is the base URL of the signaling server (must be HTTPS).
	// The peer polls {SignalingURL}/signal and posts to it.
	SignalingURL string

	// Token is the bearer token for authenticating with the signaling server.
	Token string

	// BasePath is the server's URL base path (e.g. "/ui"), used to validate
	// and dispatch incoming data-channel request paths.
	BasePath string

	// STUNURLs is a list of STUN server URLs.
	// Defaults to Google's public STUN server if empty.
	STUNURLs []string

	// PollInterval is how often to poll the signaling server for new offers.
	// Defaults to 2 seconds.
	PollInterval time.Duration

	// IdleTimeout is how long a peer connection may sit idle before being closed.
	// Defaults to 30 minutes.
	IdleTimeout time.Duration

	// MaxConns is the maximum number of concurrent WebRTC connections.
	// Defaults to 10.
	MaxConns int
}

// Diagnostics contains correlation-safe WebRTC admission counters.
type Diagnostics struct {
	Active   int32  `json:"active"`
	Reserved int32  `json:"reserved"`
	Rejected uint64 `json:"rejected"`
}

// Peer is a running WebRTC home peer. Call Close to stop it.
type Peer interface {
	Close() error
	Diagnostics() Diagnostics
}
